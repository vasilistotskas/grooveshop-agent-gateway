package acp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Handler serves the merchant-side Agentic Checkout REST API. Platform
// access is bearer-authenticated with the tenant's own token (issued at
// enrollment, served on tenant/resolve as acpBearerToken).
type Handler struct {
	dj    *django.Client
	store *checkout.Store
	flow  *checkout.Flow
	rdb   *redis.Client
	site  storefront.Site
	log   *slog.Logger
}

func NewHandler(
	dj *django.Client, store *checkout.Store, flow *checkout.Flow,
	rdb *redis.Client, site storefront.Site, log *slog.Logger,
) *Handler {
	return &Handler{
		dj: dj, store: store, flow: flow, rdb: rdb, site: site, log: log,
	}
}

// Register mounts the ACP routes. The tenant middleware wraps each route
// inside routing so patterns stay visible to metrics.
func (h *Handler) Register(mux *http.ServeMux, mw func(http.Handler) http.Handler) {
	wrap := func(fn http.HandlerFunc) http.Handler {
		return mw(h.auth(fn))
	}
	mux.Handle("POST /acp/checkout_sessions", wrap(h.create))
	mux.Handle("GET /acp/checkout_sessions/{id}", wrap(h.get))
	mux.Handle("POST /acp/checkout_sessions/{id}", wrap(h.update))
	mux.Handle("POST /acp/checkout_sessions/{id}/complete", wrap(h.complete))
	mux.Handle("POST /acp/checkout_sessions/{id}/cancel", wrap(h.cancel))
}

// auth gates a route on the requesting tenant's bearer token, so a
// platform enrolled with one store can never drive another store's
// checkout by switching Host. An empty acpBearerToken means ACP is
// disabled for the tenant: every bearer gets the same 401 as a wrong
// token.
func (h *Handler) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := tenant.FromContext(r.Context())
		if !ok {
			writeError(w, http.StatusNotFound, Error{
				Type: "invalid_request", Code: "not_found",
				Message: "Unknown store.",
			})
			return
		}
		expected := t.ACPBearerToken
		token, ok := httpmw.BearerToken(r)
		if expected == "" || !ok || subtle.ConstantTimeCompare(
			[]byte(token), []byte(expected)) != 1 {
			writeError(w, http.StatusUnauthorized, Error{
				Type: "invalid_request", Code: "unauthorized",
				Message: "A valid bearer token is required.",
			})
			return
		}
		next(w, r)
	})
}

func writeError(w http.ResponseWriter, status int, e Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

// writeUnavailable is the protocol body for a transient gateway-side
// failure (Redis, rendering); the platform retries.
func writeUnavailable(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusServiceUnavailable, Error{
		Type: "service_unavailable", Message: msg,
	})
}

// writePlacedUnrendered answers for an order that exists while its
// session is not rendered: the platform must read it again, not order
// again.
func writePlacedUnrendered(w http.ResponseWriter, s *checkout.Session) {
	writeUnavailable(w, "Order "+s.OrderUUID+" is placed, but the "+
		"session cannot be rendered right now. Read the session "+
		"again, and do not start a new checkout.")
}

func writeSession(w http.ResponseWriter, status int, s *Session) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(s)
}

// upstreamError maps Django client failures onto the protocol Error body.
func upstreamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, django.ErrNotFound),
		errors.Is(err, django.ErrCartUnreachable):
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "not_found",
			Message: "A referenced resource was not found.",
		})
	case errors.Is(err, django.ErrThrottled):
		writeError(w, http.StatusTooManyRequests, Error{
			Type: "service_unavailable", Code: "rate_limited",
			Message: "Too many requests; retry shortly.",
		})
	case errors.Is(err, django.ErrValidation),
		errors.Is(err, django.ErrForbidden),
		errors.Is(err, django.ErrUnauthorized):
		msg := "The store rejected the request as invalid."
		if apiErr, ok := errors.AsType[*django.APIError](err); ok &&
			apiErr.Detail != "" {
			msg = apiErr.Detail
		}
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "invalid", Message: msg,
		})
	default:
		writeError(w, http.StatusServiceUnavailable, Error{
			Type: "service_unavailable", Code: "upstream_unavailable",
			Message: "The store backend is temporarily unavailable.",
		})
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(
		http.MaxBytesReader(w, r.Body, 256<<10),
	).Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "invalid",
			Message: "The request body is not valid JSON.",
		})
		return false
	}
	return true
}

// --- Idempotency -----------------------------------------------------------

type idemRecord struct {
	SessionID string `json:"sessionId"`
	BodyHash  string `json:"bodyHash"`
	Status    int    `json:"status"`
}

// idemRedisKey scopes a key to one endpoint: "create", or an operation
// on one session ("complete:<id>"). The spec scopes keys per endpoint, and
// a key reused on another session is a new request, not a replay of the
// first session's response.
func idemRedisKey(schema, scope, key string) string {
	return "ag:" + schema + ":acp:idem:" + scope + ":" + key
}

func bodyHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// maxIdempotencyKeyLen is the spec's bound on the header.
const maxIdempotencyKeyLen = 255

// idemRetryAfter is the Retry-After (seconds) sent with a 409 for a key
// whose first request is still running.
const idemRetryAfter = "2"

func writeInFlight(w http.ResponseWriter) {
	w.Header().Set("Retry-After", idemRetryAfter)
	writeError(w, http.StatusConflict, Error{
		Type: "invalid_request", Code: "idempotency_in_flight",
		Message: "A request with this Idempotency-Key is in flight.",
	})
}

// claimIdem implements POST idempotency: the first caller claims the key,
// replays return the stored record, concurrent attempts get 409, and a
// different body under the same key gets 422.
func (h *Handler) claimIdem(
	w http.ResponseWriter, r *http.Request, schema, scope string, raw []byte,
) (key string, claimed bool, prior *idemRecord, handled bool) {
	key = r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "idempotency_key_required",
			Message: "The Idempotency-Key header is required.",
		})
		return "", false, nil, true
	}
	if len(key) > maxIdempotencyKeyLen {
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "invalid",
			Param:   "Idempotency-Key",
			Message: "The Idempotency-Key header exceeds 255 characters.",
		})
		return "", false, nil, true
	}
	redisKey := idemRedisKey(schema, scope, key)
	ok, err := h.rdb.SetNX(r.Context(), redisKey, "inflight", time.Minute).
		Result()
	if err != nil {
		writeUnavailable(w, "Checkout is temporarily unavailable.")
		return "", false, nil, true
	}
	if ok {
		w.Header().Set("Idempotency-Key", key)
		return key, true, nil, false
	}
	stored, err := h.rdb.Get(r.Context(), redisKey).Result()
	if err != nil || stored == "inflight" {
		writeInFlight(w)
		return "", false, nil, true
	}
	var rec idemRecord
	if json.Unmarshal([]byte(stored), &rec) != nil {
		writeInFlight(w)
		return "", false, nil, true
	}
	if rec.BodyHash != bodyHash(raw) {
		writeError(w, http.StatusUnprocessableEntity, Error{
			Type: "invalid_request", Code: "idempotency_conflict",
			Message: "This Idempotency-Key was used with a different body.",
		})
		return "", false, nil, true
	}
	w.Header().Set("Idempotency-Key", key)
	w.Header().Set("Idempotent-Replayed", "true")
	return key, false, &rec, false
}

// storeIdem records what a claimed key did. Like releaseIdem it settles
// the key even for a platform that has gone: a key left in flight
// answers its retry 409 for a minute.
func (h *Handler) storeIdem(
	r *http.Request, schema, scope, key string, rec idemRecord,
) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := h.rdb.Set(context.WithoutCancel(r.Context()),
		idemRedisKey(schema, scope, key), raw, 24*time.Hour).
		Err(); err != nil {
		h.logUnsettled(r, scope, "record", err)
	}
}

// releaseIdem clears an inflight claim after a failed attempt.
func (h *Handler) releaseIdem(r *http.Request, schema, scope, key string) {
	if err := h.rdb.Del(context.WithoutCancel(r.Context()),
		idemRedisKey(schema, scope, key)).Err(); err != nil {
		h.logUnsettled(r, scope, "release", err)
	}
}

// logUnsettled reports a key left as its claim wrote it: "inflight" for
// a minute, which answers the platform's retry 409 until it lapses.
func (h *Handler) logUnsettled(
	r *http.Request, scope, action string, err error,
) {
	h.log.ErrorContext(r.Context(), "acp: idempotency key not settled",
		slog.String("scope", scope), slog.String("action", action),
		slog.String("error", err.Error()))
}

// --- Input mapping ---------------------------------------------------------

// applyBuyer folds ACP buyer + fulfillment contact details onto the
// session (ACP requires only the email; names may arrive on the
// fulfillment contact instead).
func applyBuyer(
	s *checkout.Session, b *Buyer, fd *FulfillmentDetails,
) {
	if b != nil {
		if b.FirstName != "" {
			s.Buyer.FirstName = b.FirstName
		}
		if b.LastName != "" {
			s.Buyer.LastName = b.LastName
		}
		if b.Email != "" {
			s.Buyer.Email = b.Email
		}
		if b.PhoneNumber != "" {
			s.Buyer.Phone = b.PhoneNumber
		}
	}
	if fd == nil {
		return
	}
	if s.Buyer.Phone == "" && fd.PhoneNumber != "" {
		s.Buyer.Phone = fd.PhoneNumber
	}
	if s.Buyer.Email == "" && fd.Email != "" {
		s.Buyer.Email = fd.Email
	}
	if s.Buyer.FirstName == "" && fd.Name != "" {
		first, last, _ := strings.Cut(fd.Name, " ")
		s.Buyer.FirstName = first
		if s.Buyer.LastName == "" && last != "" {
			s.Buyer.LastName = last
		}
	}
}

func applyFulfillment(
	s *checkout.Session, fd *FulfillmentDetails,
	selected []SelectedFulfillmentOption,
) {
	if fd != nil && fd.Address != nil {
		a := fd.Address
		// ACP addresses are free-form lines while Django's order model
		// requires a separate street_number — recover it from a
		// trailing numeric token ("Ερμού 12" → "Ερμού" + "12").
		street, number := splitStreetNumber(a.LineOne)
		s.Fulfillment.Street = street
		s.Fulfillment.StreetNumber = number
		if a.LineTwo != "" {
			s.Fulfillment.Street += ", " + a.LineTwo
		}
		s.Fulfillment.City = a.City
		s.Fulfillment.Zipcode = a.PostalCode
		s.Fulfillment.CountryCode = a.Country
	}
	for _, sel := range selected {
		provider, kind, ok := strings.Cut(sel.OptionID, ":")
		if ok && kind == checkout.FulfillmentHomeDelivery {
			s.Fulfillment.ProviderCode = provider
			s.Fulfillment.Kind = kind
		}
	}
}

// splitStreetNumber cuts a trailing house-number token off a free-form
// address line ("Ερμού 12" → "Ερμού", "12"; "Ερμού 12Β" keeps the
// letter suffix). Lines without a numeric tail stay whole — Django then
// reports street_number as missing, which is the store's own rule for
// human checkouts too.
func splitStreetNumber(line string) (street, number string) {
	line = strings.TrimSpace(line)
	i := strings.LastIndexByte(line, ' ')
	if i <= 0 {
		return line, ""
	}
	tail := line[i+1:]
	if tail == "" || tail[0] < '0' || tail[0] > '9' {
		return line, ""
	}
	return strings.TrimSpace(line[:i]), tail
}

// cartLines maps ACP item refs onto product lines.
func cartLines(items []ItemRef) ([]checkout.Line, error) {
	lines := make([]checkout.Line, 0, len(items))
	for _, ref := range items {
		productID, err := strconv.ParseInt(ref.ID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: unknown item id %q",
				django.ErrNotFound, ref.ID)
		}
		// The spec's request item has no quantity, so a platform that
		// follows it buys one. One it does send is the store's to refuse.
		qty := 1
		if ref.Quantity != nil {
			qty = *ref.Quantity
		}
		lines = append(lines,
			checkout.Line{ProductID: productID, Quantity: qty})
	}
	return lines, nil
}

func (h *Handler) syncCart(
	r *http.Request, t *tenant.Tenant, cartID string, items []ItemRef,
) error {
	lines, err := cartLines(items)
	if err != nil {
		return err
	}
	return checkout.SyncCartLines(
		r.Context(), h.dj, t, checkout.ProtocolACP, cartID, lines)
}

// selectPayWay picks the collect-later method completion will use as soon
// as the delivery is known. Pricing adds a payment-method fee only for a
// selected pay way, so without this the ready_for_payment total the buyer
// approved lacked the cash-on-delivery fee the order then charged.
func (h *Handler) selectPayWay(
	r *http.Request, t *tenant.Tenant, s *checkout.Session,
) error {
	s.PayWayID = 0
	if !s.Fulfillment.Complete() {
		return nil
	}
	pw, err := checkout.OfflinePayWay(r.Context(), h.dj, t, s.Fulfillment)
	if err != nil {
		return err
	}
	if pw != nil {
		s.PayWayID = pw.ID
	}
	return nil
}

// applyRegion settles the delivery region from the submitted address's
// state, whenever one was submitted: its country decides whether a region
// is needed at all.
func (h *Handler) applyRegion(
	r *http.Request, t *tenant.Tenant, s *checkout.Session,
	fd *FulfillmentDetails,
) error {
	if fd == nil || fd.Address == nil {
		return nil
	}
	return checkout.ResolveRegion(r.Context(), h.dj, t, s, fd.Address.State)
}

// --- Endpoints -------------------------------------------------------------

func (h *Handler) tenantOf(w http.ResponseWriter, r *http.Request) (
	*tenant.Tenant, bool,
) {
	t, ok := tenant.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusNotFound, Error{
			Type: "invalid_request", Code: "not_found",
			Message: "Unknown store.",
		})
		return nil, false
	}
	return t, true
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenantOf(w, r)
	if !ok {
		return
	}
	var req CreateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	raw, _ := json.Marshal(req)

	key, claimed, prior, handled := h.claimIdem(w, r, t.SchemaName,
		"create", raw)
	if handled {
		return
	}
	if !claimed {
		s, err := h.store.Load(r.Context(), t.SchemaName, prior.SessionID)
		if err != nil {
			writeError(w, http.StatusConflict, Error{
				Type: "invalid_request", Code: "idempotency_conflict",
				Message: "The original session for this key has expired.",
			})
			return
		}
		h.render(w, r, t, s, prior.Status)
		return
	}

	if len(req.LineItems) == 0 {
		h.releaseIdem(r, t.SchemaName, "create", key)
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "missing",
			Param: "$.line_items", Message: "line_items must not be empty.",
		})
		return
	}
	// Refused, not converted: amounts the platform reads as its own
	// currency would be the store's.
	if !strings.EqualFold(req.Currency, t.DefaultCurrency) {
		h.releaseIdem(r, t.SchemaName, "create", key)
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "invalid", Param: "$.currency",
			Message: "This store sells in " + t.DefaultCurrency + ".",
		})
		return
	}

	cart, err := h.dj.GetCart(r.Context(), t.Domain, t.DefaultLocale, "")
	if err != nil {
		h.releaseIdem(r, t.SchemaName, "create", key)
		upstreamError(w, err)
		return
	}
	if err := h.syncCart(r, t, cart.UUID, req.LineItems); err != nil {
		h.releaseIdem(r, t.SchemaName, "create", key)
		upstreamError(w, err)
		return
	}

	s := checkout.NewSession(t.SchemaName, t.Domain, checkout.ProtocolACP, cart.UUID)
	// Discounts apply after the lines exist (eligibility can depend on
	// the cart subtotal). A refused code is not a request failure — it
	// renders in discounts.rejected.
	if req.Discounts != nil && req.Discounts.Codes != nil {
		if err := checkout.ApplyDiscountCodes(
			r.Context(), h.dj, t, s, req.Discounts.Codes,
		); err != nil {
			h.releaseIdem(r, t.SchemaName, "create", key)
			upstreamError(w, err)
			return
		}
	}
	applyBuyer(s, req.Buyer, req.FulfillmentDetails)
	applyFulfillment(s, req.FulfillmentDetails, nil)
	if err := h.applyRegion(r, t, s, req.FulfillmentDetails); err != nil {
		h.releaseIdem(r, t.SchemaName, "create", key)
		upstreamError(w, err)
		return
	}
	if err := h.selectPayWay(r, t, s); err != nil {
		h.releaseIdem(r, t.SchemaName, "create", key)
		upstreamError(w, err)
		return
	}
	s.Recompute()
	if err := h.store.Save(r.Context(), s); err != nil {
		h.releaseIdem(r, t.SchemaName, "create", key)
		writeUnavailable(w, "Checkout is temporarily unavailable.")
		return
	}
	h.storeIdem(r, t.SchemaName, "create", key, idemRecord{
		SessionID: s.ID, BodyHash: bodyHash(raw),
		Status: http.StatusCreated,
	})
	h.render(w, r, t, s, http.StatusCreated)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenantOf(w, r)
	if !ok {
		return
	}
	s, err := h.store.Load(r.Context(), t.SchemaName, r.PathValue("id"))
	if err != nil {
		h.sessionError(w, err)
		return
	}
	h.render(w, r, t, s, http.StatusOK)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenantOf(w, r)
	if !ok {
		return
	}
	var req UpdateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	raw, _ := json.Marshal(req)
	scope := "update:" + r.PathValue("id")
	key, claimed, _, handled := h.claimIdem(w, r, t.SchemaName, scope, raw)
	if handled {
		return
	}
	fail := func() {
		if claimed {
			h.releaseIdem(r, t.SchemaName, scope, key)
		}
	}
	s, release, ok := h.lockSession(w, r, t, r.PathValue("id"))
	if !ok {
		fail()
		return
	}
	defer release()
	if !claimed {
		h.render(w, r, t, s, http.StatusOK)
		return
	}
	if s.Status == checkout.StatusCompleteInProgress {
		fail()
		writeInFlight(w)
		return
	}
	if s.Terminal() {
		fail()
		writeError(w, http.StatusConflict, Error{
			Type: "invalid_request", Code: "conflict",
			Message: fmt.Sprintf("Session is %s and cannot change.", s.Status),
		})
		return
	}

	// Before any cart write: a failed lookup must leave the cart and the
	// stored session agreeing. Nothing here is saved until the end.
	applyBuyer(s, req.Buyer, req.FulfillmentDetails)
	applyFulfillment(s, req.FulfillmentDetails,
		req.SelectedFulfillmentOptions)
	if err := h.applyRegion(r, t, s, req.FulfillmentDetails); err != nil {
		fail()
		upstreamError(w, err)
		return
	}
	if req.LineItems != nil {
		if err := h.syncCart(r, t, s.CartID, req.LineItems); err != nil {
			fail()
			upstreamError(w, err)
			return
		}
	}
	if req.Discounts != nil && req.Discounts.Codes != nil {
		if err := checkout.ApplyDiscountCodes(
			r.Context(), h.dj, t, s, req.Discounts.Codes,
		); err != nil {
			fail()
			upstreamError(w, err)
			return
		}
	}
	if err := h.selectPayWay(r, t, s); err != nil {
		fail()
		upstreamError(w, err)
		return
	}
	s.Recompute()
	if err := h.store.Save(r.Context(), s); err != nil {
		fail()
		writeUnavailable(w, "Checkout is temporarily unavailable.")
		return
	}
	h.storeIdem(r, t.SchemaName, scope, key, idemRecord{
		SessionID: s.ID, BodyHash: bodyHash(raw), Status: http.StatusOK,
	})
	h.render(w, r, t, s, http.StatusOK)
}

func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenantOf(w, r)
	if !ok {
		return
	}
	var req CompleteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	raw, _ := json.Marshal(req)

	s, release, ok := h.lockSession(w, r, t, r.PathValue("id"))
	if !ok {
		return
	}
	defer release()

	switch s.Status {
	case checkout.StatusCompleted:
		h.render(w, r, t, s, http.StatusOK)
		return
	case checkout.StatusCompleteInProgress:
		writeInFlight(w)
		return
	}

	scope := "complete:" + s.ID
	key, claimed, _, handled := h.claimIdem(w, r, t.SchemaName, scope, raw)
	if handled {
		return
	}
	if !claimed {
		// A replayed key re-renders whatever the first attempt produced;
		// the stored record only matters for create, which must name
		// the session it minted.
		h.render(w, r, t, s, http.StatusOK)
		return
	}

	if req.PaymentData == nil {
		h.releaseIdem(r, t.SchemaName, scope, key)
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "missing",
			Param: "$.payment_data", Message: "payment_data is required.",
		})
		return
	}
	// Delegated payment is not implemented: any payment handler or
	// tokenized instrument (Stripe SPT included) is refused. Card buyers
	// pay on the store's checkout via continue_url; only handler-less
	// completion (a collect-later pay way such as cash on delivery)
	// works here.
	if req.PaymentData.HandlerID != "" ||
		len(req.PaymentData.Instrument) > 0 {
		h.releaseIdem(r, t.SchemaName, scope, key)
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "unsupported",
			Message: "Delegated card payment is not supported. To pay " +
				"by card, send the buyer to continue_url: the store's " +
				"checkout takes the card on its payment provider's " +
				"hosted page. To complete here, send empty " +
				"payment_data and the order is placed with the store's " +
				"pay-later method, such as cash on delivery.",
		})
		return
	}

	buyer, status := s.Buyer, s.Status
	applyBuyer(s, req.Buyer, nil)
	s.Recompute()
	// The buyer this call submitted is the latest word whether or not
	// the call places the order. It is stored now, because a refused
	// completion stores nothing: a later complete that omits the buyer
	// would otherwise place the order for the one before. No Django call
	// sits between the load and this save, so the lock still holds. The
	// pay way is not part of it: it is looked up again on every call.
	if s.Open() && (s.Buyer != buyer || s.Status != status) {
		if err := h.store.Save(r.Context(), s); err != nil {
			h.releaseIdem(r, t.SchemaName, scope, key)
			writeUnavailable(w, "Checkout is temporarily unavailable.")
			return
		}
	}
	// Readiness first, and it needs no pay way on ACP. Without a delivery no
	// pay way can be chosen, which says nothing about what the store offers.
	if s.Status == checkout.StatusIncomplete {
		h.releaseIdem(r, t.SchemaName, scope, key)
		h.completeError(w, checkout.ErrNotReady)
		return
	}
	if err := h.selectPayWay(r, t, s); err != nil {
		h.releaseIdem(r, t.SchemaName, scope, key)
		upstreamError(w, err)
		return
	}
	if s.PayWayID == 0 {
		h.releaseIdem(r, t.SchemaName, scope, key)
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "unsupported",
			Message: "This store has no agent-completable payment " +
				"method; send the buyer to continue_url.",
		})
		return
	}

	record := idemRecord{
		SessionID: s.ID, BodyHash: bodyHash(raw), Status: http.StatusOK,
	}
	placement, err := h.flow.Complete(r.Context(), t, s)
	if placement == checkout.NothingPlaced {
		h.releaseIdem(r, t.SchemaName, scope, key)
	} else {
		h.storeIdem(r, t.SchemaName, scope, key, record)
	}
	if err != nil {
		h.completeError(w, err)
		return
	}
	if r.Context().Err() != nil {
		// The platform looks gone, so reading the order back on its
		// ended request could only fail. A half-closed connection looks
		// the same and still reads the answer: it is the one a failed
		// render gives.
		writePlacedUnrendered(w, s)
		return
	}
	h.render(w, r, t, s, http.StatusOK)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenantOf(w, r)
	if !ok {
		return
	}
	scope := "cancel:" + r.PathValue("id")
	key, claimed, _, handled := h.claimIdem(w, r, t.SchemaName, scope,
		[]byte(r.PathValue("id")))
	if handled {
		return
	}
	s, release, ok := h.lockSession(w, r, t, r.PathValue("id"))
	if !ok {
		if claimed {
			h.releaseIdem(r, t.SchemaName, scope, key)
		}
		return
	}
	defer release()

	if !claimed {
		h.render(w, r, t, s, http.StatusOK)
		return
	}
	if s.Status == checkout.StatusCompleteInProgress {
		h.releaseIdem(r, t.SchemaName, scope, key)
		writeInFlight(w)
		return
	}
	if s.Terminal() {
		h.releaseIdem(r, t.SchemaName, scope, key)
		writeError(w, http.StatusMethodNotAllowed, Error{
			Type: "invalid_request", Code: "conflict",
			Message: fmt.Sprintf("Session is already %s.", s.Status),
		})
		return
	}
	s.Status = checkout.StatusCanceled
	if err := h.store.Save(r.Context(), s); err != nil {
		h.releaseIdem(r, t.SchemaName, scope, key)
		writeUnavailable(w, "Checkout is temporarily unavailable.")
		return
	}
	h.storeIdem(r, t.SchemaName, scope, key, idemRecord{
		SessionID: s.ID, BodyHash: bodyHash([]byte(r.PathValue("id"))),
		Status: http.StatusOK,
	})
	h.render(w, r, t, s, http.StatusOK)
}

// --- Shared plumbing -------------------------------------------------------

func (h *Handler) lockSession(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant, id string,
) (*checkout.Session, func(), bool) {
	release, err := h.store.Lock(r.Context(), t.SchemaName, id)
	if err != nil {
		if errors.Is(err, checkout.ErrLocked) {
			writeError(w, http.StatusConflict, Error{
				Type: "invalid_request", Code: "conflict",
				Message: "The session is busy; retry shortly.",
			})
			return nil, nil, false
		}
		writeUnavailable(w, "Checkout is temporarily unavailable.")
		return nil, nil, false
	}
	s, err := h.store.Load(r.Context(), t.SchemaName, id)
	if err != nil {
		release()
		h.sessionError(w, err)
		return nil, nil, false
	}
	return s, release, true
}

func (h *Handler) sessionError(w http.ResponseWriter, err error) {
	if errors.Is(err, checkout.ErrNotFound) {
		writeError(w, http.StatusNotFound, Error{
			Type: "invalid_request", Code: "not_found",
			Message: "No such checkout session.",
		})
		return
	}
	writeUnavailable(w, "Checkout is temporarily unavailable.")
}

func (h *Handler) completeError(w http.ResponseWriter, err error) {
	var shortfall *django.StockShortfall
	switch {
	case errors.As(err, &shortfall):
		var lines []string
		for _, item := range shortfall.FailedItems {
			lines = append(lines, fmt.Sprintf(
				"%s: requested %d, available %d",
				item.ProductName, item.Requested, item.Available))
		}
		writeError(w, http.StatusConflict, Error{
			Type: "processing_error", Code: "out_of_stock",
			Message: "Not enough stock: " + strings.Join(lines, "; "),
		})
	case errors.Is(err, checkout.ErrNotReady):
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "missing",
			Message: "The session is missing buyer or fulfillment data.",
		})
	case errors.Is(err, checkout.ErrCompletionInProgress):
		writeInFlight(w)
	case errors.Is(err, checkout.ErrOrderOutcomeUnknown):
		writeError(w, http.StatusBadGateway, Error{
			Type: "processing_error", Code: "order_outcome_unknown",
			Message: "The store did not confirm whether the order was " +
				"placed; do not retry — retrieve the session later.",
		})
	case errors.Is(err, checkout.ErrSessionNotSaved):
		writeUnavailable(w, "The order was placed but the session could not be "+
			"saved; retrieve it again shortly.")
	case errors.Is(err, checkout.ErrPayWayUnavailable):
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "unsupported",
			Message: "No agent-completable payment method serves this " +
				"delivery; send the buyer to continue_url.",
		})
	default:
		upstreamError(w, err)
	}
}

func (h *Handler) render(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant,
	s *checkout.Session, status int,
) {
	// ACP names no language: the store's default.
	payload, err := Render(r.Context(), h.dj, t,
		h.site.Links(t.Domain, t.DefaultLocale), s)
	if err != nil {
		h.log.ErrorContext(r.Context(), "acp render failed",
			slog.String("session", s.ID), slog.String("error", err.Error()))
		if s.OrderUUID != "" {
			writePlacedUnrendered(w, s)
			return
		}
		writeUnavailable(w, "The session cannot be rendered right now.")
		return
	}
	writeSession(w, status, payload)
}
