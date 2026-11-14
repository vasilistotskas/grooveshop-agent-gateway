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
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

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
	site  storefront.Site
	log   *slog.Logger
}

func NewHandler(
	dj *django.Client, store *checkout.Store, flow *checkout.Flow,
	site storefront.Site, log *slog.Logger,
) *Handler {
	return &Handler{dj: dj, store: store, flow: flow, site: site, log: log}
}

// Register mounts the ACP routes. The tenant middleware wraps each route
// inside routing so patterns stay visible to metrics.
func (h *Handler) Register(mux *http.ServeMux, mw func(http.Handler) http.Handler) {
	wrap := func(fn http.HandlerFunc) http.Handler {
		return mw(h.auth(requireVersion(fn)))
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

// requireVersion refuses a request whose API-Version this server does not
// implement: the spec has the client send it and the server validate it,
// answering 400 with the versions it accepts.
func requireVersion(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version := r.Header.Get("API-Version")
		if slices.Contains(supportedVersions, version) {
			next(w, r)
			return
		}
		e := Error{
			Type: "invalid_request", Code: "unsupported_api_version",
			Message:           "This API-Version is not supported.",
			SupportedVersions: supportedVersions,
		}
		if version == "" {
			e.Code = "missing_api_version"
			e.Message = "The API-Version header is required."
		}
		writeError(w, http.StatusBadRequest, e)
	}
}

func writeError(w http.ResponseWriter, status int, e Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

// msgRecordsUnavailable answers a checkout store (Redis) that could not
// be read or written: sessions, locks, idempotency keys. It names the
// gateway's own records, never the store's backend.
const msgRecordsUnavailable = "Checkout is temporarily unavailable."

// writeUnavailable is the protocol body for a transient gateway-side
// failure (Redis, rendering); the platform retries.
func writeUnavailable(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusServiceUnavailable, Error{
		Type: "service_unavailable", Code: "unavailable", Message: msg,
	})
}

// writePlacedUnrendered answers for an order that exists while its
// session is not rendered: the platform must read it again, not order
// again.
func writePlacedUnrendered(w http.ResponseWriter, s *checkout.Session) {
	writeError(w, http.StatusServiceUnavailable, Error{
		Type: "service_unavailable", Code: "order_placed",
		Message: "Order " + s.OrderUUID + " is placed, but the " +
			"session cannot be rendered right now. Read the session " +
			"again, and do not start a new checkout.",
	})
}

// writeOutcomeUnknown answers for a session whose order the store never
// confirmed or denied: it stays complete_in_progress until it expires, so
// no retry changes the answer, and a new checkout could order twice.
func writeOutcomeUnknown(w http.ResponseWriter) {
	writeError(w, http.StatusBadGateway, Error{
		Type: "processing_error", Code: "order_outcome_unknown",
		Message: "The store did not confirm whether the order was " +
			"placed. Do not complete again or start a new checkout: " +
			"the buyer receives the store's confirmation email if it was.",
	})
}

// writeClosed refuses to change a completed or canceled session. The spec
// answers a cancel of one 405; an update or a complete of one is the same
// refusal. allow is what the target still serves, which a 405 must name
// (RFC 9110 §15.5.6).
func writeClosed(w http.ResponseWriter, allow, msg string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, Error{
		Type: "invalid_request", Code: "conflict", Message: msg,
	})
}

func writeBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
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
		errors.Is(err, django.ErrConflict),
		errors.Is(err, django.ErrForbidden),
		errors.Is(err, django.ErrUnauthorized),
		errors.Is(err, django.ErrInvalidCartToken):
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

// idemRecord is what a settled key did. A create's holds the response it
// sent, which its replays answer byte for byte; update, complete and
// cancel replays render the session as it is now.
type idemRecord struct {
	BodyHash string `json:"bodyHash"`
	Status   int    `json:"status,omitempty"`
	Body     string `json:"body,omitempty"`
}

const (
	// idemRecordTTL is the spec's minimum retention of a settled key.
	idemRecordTTL = 24 * time.Hour
	// createScope is the create endpoint's scope, versioned with the
	// record it stores: bump it when that shape changes, so a record an
	// older pod wrote is a miss, never misread.
	createScope = "create.v2"
)

// idemRedisKey scopes a key to one endpoint: "create.v2", or an operation
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

// idemRetryAfter is the Retry-After sent with a 409 for a key whose first
// request, or another request on the same session, is still running.
const idemRetryAfter = 2 * time.Second

const (
	msgKeyInFlight = "A request with this Idempotency-Key is in flight."
	msgSessionBusy = "Another request on this checkout session is in " +
		"flight."
	msgSessionChanged = "Another request changed this checkout session " +
		"while this one ran. Send the request again."
)

// writeInFlight is the spec's 409: the platform retries the same request
// after the Retry-After it requires, in whole seconds.
func writeInFlight(w http.ResponseWriter, after time.Duration, msg string) {
	w.Header().Set("Retry-After",
		strconv.Itoa(int(math.Ceil(after.Seconds()))))
	writeError(w, http.StatusConflict, Error{
		Type: "invalid_request", Code: "idempotency_in_flight",
		Message: msg,
	})
}

// writePlacedUnsaved answers for an order that is placed while its
// session never recorded it: to the completion whose last save failed,
// and to every later call, which finds the session in progress and the
// order named beside it. Not temporary: the session stays as it is.
func writePlacedUnsaved(w http.ResponseWriter, orderUUID string) {
	writeError(w, http.StatusInternalServerError, Error{
		Type: "processing_error", Code: "order_placed",
		Message: "Order " + orderUUID + " is placed, but its " +
			"checkout session could not be saved. Do not complete " +
			"again or start a new checkout.",
	})
}

// writeCompletionPending answers a call that finds the session's order
// being placed. An order the completion placed and could not save is
// known (the session or the store names it): that is the answer. Else,
// while the completion can still run, retry once it must have ended;
// past that, its outcome was never recorded.
func (h *Handler) writeCompletionPending(
	w http.ResponseWriter, r *http.Request, s *checkout.Session,
) {
	if order := h.flow.PlacedOrder(r.Context(), s); order != "" {
		writePlacedUnsaved(w, order)
		return
	}
	if left := time.Until(h.flow.CompletionDeadline(s)); left > 0 {
		writeInFlight(w, left,
			"This checkout session's order is being placed.")
		return
	}
	writeOutcomeUnknown(w)
}

// claimKey implements POST idempotency: the first caller claims the key,
// replays return the stored record, concurrent attempts get 409, and a
// different body under the same key gets 422. It returns the claim when
// this request holds the key, the record when an earlier one settled
// it, and otherwise the refusal to answer with.
func (h *Handler) claimKey(
	r *http.Request, schema, scope string, raw []byte,
) (
	key string, claim *checkout.Claim, prior *idemRecord,
	refuse func(http.ResponseWriter),
) {
	refusal := func(status int, e Error) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) { writeError(w, status, e) }
	}
	key = r.Header.Get("Idempotency-Key")
	if key == "" {
		return "", nil, nil, refusal(http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "idempotency_key_required",
			Message: "The Idempotency-Key header is required.",
		})
	}
	if len(key) > maxIdempotencyKeyLen {
		return "", nil, nil, refusal(http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "invalid",
			Param:   "Idempotency-Key",
			Message: "The Idempotency-Key header exceeds 255 characters.",
		})
	}
	inFlight := func(w http.ResponseWriter) {
		writeInFlight(w, idemRetryAfter, msgKeyInFlight)
	}
	claim, settled, err := h.store.Claim(r.Context(),
		idemRedisKey(schema, scope, key))
	if err != nil {
		return "", nil, nil, func(w http.ResponseWriter) {
			writeUnavailable(w, msgRecordsUnavailable)
		}
	}
	if claim != nil {
		return key, claim, nil, nil
	}
	// Held by another request, or settled with something that is not a
	// record: in flight either way.
	var rec idemRecord
	if settled == "" || json.Unmarshal([]byte(settled), &rec) != nil {
		return "", nil, nil, inFlight
	}
	if rec.BodyHash != bodyHash(raw) {
		return "", nil, nil, refusal(http.StatusUnprocessableEntity, Error{
			Type: "invalid_request", Code: "idempotency_conflict",
			Message: "This Idempotency-Key was used with a different body.",
		})
	}
	return key, nil, &rec, nil
}

// claimIdem is claimKey for a handler that holds no session yet: it
// answers the refusal itself, and echoes a key that was accepted.
func (h *Handler) claimIdem(
	w http.ResponseWriter, r *http.Request, schema, scope string, raw []byte,
) (claim *checkout.Claim, prior *idemRecord, handled bool) {
	key, claim, prior, refuse := h.claimKey(r, schema, scope, raw)
	if refuse != nil {
		refuse(w)
		return nil, nil, true
	}
	w.Header().Set("Idempotency-Key", key)
	return claim, prior, false
}

// writeReplay answers a replayed key with what the spec asks of it.
func writeReplay(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Idempotent-Replayed", "true")
	writeBody(w, status, body)
}

// settling is the context a claimed key is recorded or released on. It
// outlives the request's own (a platform that hung up, a shutdown): a
// key left in flight answers the retry 409 until its claim lapses. The
// Django client's timeout bounds the one Redis write, which go-redis
// alone would retry against a silent Redis for several times that.
func (h *Handler) settling(r *http.Request) (
	context.Context, context.CancelFunc,
) {
	return context.WithTimeout(
		context.WithoutCancel(r.Context()), h.dj.Timeout())
}

// errClaimLapsed is logged for a key its request could not record: the
// request ran past its claim, and another request holds the key or has
// settled it.
var errClaimLapsed = errors.New(
	"the claim lapsed and another request has the key")

// storeIdem records what a claimed key did, and returns nil when the key
// holds that record. errClaimLapsed: the claim lapsed and another
// request took the key, so nothing was written. Any other error: the
// record may or may not be stored.
func (h *Handler) storeIdem(
	r *http.Request, claim *checkout.Claim, scope string, rec idemRecord,
) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	ctx, cancel := h.settling(r)
	defer cancel()
	recorded, err := claim.Settle(ctx, string(raw), idemRecordTTL)
	if err == nil && !recorded {
		err = errClaimLapsed
	}
	if err != nil {
		h.logUnsettled(r, scope, "record", err)
	}
	return err
}

// releaseIdem frees a claimed key after a failed attempt.
func (h *Handler) releaseIdem(
	r *http.Request, claim *checkout.Claim, scope string,
) {
	ctx, cancel := h.settling(r)
	defer cancel()
	if err := claim.Release(ctx); err != nil {
		h.logUnsettled(r, scope, "release", err)
	}
}

// logUnsettled reports a key left as its claim wrote it, in flight until
// the claim lapses, which answers the platform's retry 409 until then;
// or a key that became another request's.
func (h *Handler) logUnsettled(
	r *http.Request, scope, action string, err error,
) {
	h.log.ErrorContext(r.Context(), "acp: idempotency key not settled",
		slog.String("scope", scope), slog.String("action", action),
		slog.String("error", err.Error()))
}

// saveFailed answers a session that could not be stored: one that
// another request stored meanwhile is to be sent again, like a busy one.
func saveFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, checkout.ErrSessionChanged) {
		writeInFlight(w, idemRetryAfter, msgSessionChanged)
		return
	}
	writeUnavailable(w, msgRecordsUnavailable)
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

	claim, prior, handled := h.claimIdem(w, r, t.SchemaName,
		createScope, raw)
	if handled {
		return
	}
	if claim == nil {
		// The response that was sent, whether or not its session lives:
		// a replay re-executes nothing.
		writeReplay(w, prior.Status, []byte(prior.Body))
		return
	}

	if len(req.LineItems) == 0 {
		h.releaseIdem(r, claim, createScope)
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "missing",
			Param: "$.line_items", Message: "line_items must not be empty.",
		})
		return
	}
	// Refused, not converted: amounts the platform reads as its own
	// currency would be the store's.
	if !strings.EqualFold(req.Currency, t.DefaultCurrency) {
		h.releaseIdem(r, claim, createScope)
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "invalid", Param: "$.currency",
			Message: "This store sells in " + t.DefaultCurrency + ".",
		})
		return
	}

	cart, err := h.dj.GetCart(r.Context(), t.Domain, t.DefaultLocale, "")
	if err != nil {
		h.releaseIdem(r, claim, createScope)
		upstreamError(w, err)
		return
	}
	if err := h.syncCart(r, t, cart.UUID, req.LineItems); err != nil {
		h.releaseIdem(r, claim, createScope)
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
			h.releaseIdem(r, claim, createScope)
			upstreamError(w, err)
			return
		}
	}
	applyBuyer(s, req.Buyer, req.FulfillmentDetails)
	applyFulfillment(s, req.FulfillmentDetails, nil)
	if err := h.applyRegion(r, t, s, req.FulfillmentDetails); err != nil {
		h.releaseIdem(r, claim, createScope)
		upstreamError(w, err)
		return
	}
	if err := h.selectPayWay(r, t, s); err != nil {
		h.releaseIdem(r, claim, createScope)
		upstreamError(w, err)
		return
	}
	s.Recompute()
	// Rendered before the save, which it does not read: a session that
	// cannot be answered is not stored, and a 5xx is never stored against
	// the key, so a retry is a new create that leaves nothing behind.
	body, ok := h.renderBody(w, r, t, s)
	if !ok {
		h.releaseIdem(r, claim, createScope)
		return
	}
	// The cart was made for this session, so no checkout holds it.
	if err := h.store.Create(
		r.Context(), s, checkout.CartHolder{},
	); err != nil {
		if errors.Is(err, checkout.ErrCartHeld) {
			h.log.ErrorContext(r.Context(),
				"acp: a cart just made is already in a checkout")
		} else {
			// Whether the session was stored is not known: the script
			// may have run and its reply been lost.
			h.discard(r, s)
		}
		h.releaseIdem(r, claim, createScope)
		writeUnavailable(w, msgRecordsUnavailable)
		return
	}
	err = h.storeIdem(r, claim, createScope, idemRecord{
		BodyHash: bodyHash(raw), Status: http.StatusCreated,
		Body: string(body),
	})
	switch {
	case err == nil:
	case !errors.Is(err, errClaimLapsed):
		// Whether the key holds this response is not known, so the
		// session is not handed out: answered 201 without its record, a
		// retry of the key would create a second one. It is not
		// discarded either, since the record may name it. The retry
		// replays it if it was recorded, and creates anew if not.
		writeUnavailable(w, msgRecordsUnavailable)
		return
	default:
		// The key is another create's by now, and one key answers one
		// session: this one was never handed out, and is not kept.
		h.discard(r, s)
		writeInFlight(w, idemRetryAfter, msgKeyInFlight)
		return
	}
	writeBody(w, http.StatusCreated, body)
}

// discard removes a session its create stored, or may have stored, and
// gave to nobody, on a context the caller cannot end. Discarding one
// that was not stored removes nothing.
func (h *Handler) discard(r *http.Request, s *checkout.Session) {
	ctx, cancel := h.settling(r)
	defer cancel()
	if err := h.store.Discard(ctx, s); err != nil {
		h.log.ErrorContext(r.Context(),
			"acp: unanswered session not discarded",
			slog.String("session", s.ID),
			slog.String("error", err.Error()))
	}
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
	h.render(w, r, t, s)
}

// answer is how the body of update, complete or cancel ends. A body has
// no ResponseWriter: it can only return an answer, and only finish writes
// one. That is what keeps every way out of a handler behind the check
// finish makes, a way added later included.
//
// What a body holds is a snapshot of the session. The lock meant to keep
// it current can lapse while Django is slow, and another request may
// have placed the order since: Django then refuses this request for
// reasons that are the other request's doing (an empty cart, a line that
// is gone, stock the order took), and the snapshot describes a session
// that no longer exists.
type answer struct {
	write func(http.ResponseWriter)
	// current: the request's own save of the session was just accepted,
	// which proves its snapshot was the stored session.
	current bool
}

// fromSnapshot is an answer made from the session as the request holds
// it, or from what Django said on its behalf. finish gives it only while
// the stored session is still that one.
func fromSnapshot(write func(http.ResponseWriter)) answer {
	return answer{write: write}
}

// afterSave is an answer to a request whose save of the session was
// accepted: an update or cancel that stored, a completion past the
// in-progress save, whose outcome is its own.
func afterSave(write func(http.ResponseWriter)) answer {
	return answer{write: write, current: true}
}

// finish writes a body's answer. One made from the snapshot stands only
// if the stored session is still the revision the request holds.
// Otherwise the request is answered by what is stored: stored, by the
// same function that answers the operation for a session in that state,
// or as a session another request changed while it is still open; no
// longer stored, as missing; unreadable, as unavailable.
func (h *Handler) finish(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant,
	s *checkout.Session, a answer,
	stored func(http.ResponseWriter, *checkout.Session) bool,
) {
	if a.current {
		a.write(w)
		return
	}
	// The body may be answering a caller that left; the read is short
	// and its answer goes nowhere then.
	now, err := h.store.Load(r.Context(), t.SchemaName, s.ID)
	switch {
	case err != nil:
		h.sessionError(w, err)
	case now.Revision == s.Revision:
		a.write(w)
	case !stored(w, now):
		saveFailed(w, checkout.ErrSessionChanged)
	}
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenantOf(w, r)
	if !ok {
		return
	}
	if !checkout.ValidID(r.PathValue("id")) {
		h.sessionError(w, checkout.ErrNotFound)
		return
	}
	var req UpdateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	raw, _ := json.Marshal(req)
	scope := "update:" + r.PathValue("id")
	claim, _, handled := h.claimIdem(w, r, t.SchemaName, scope, raw)
	if handled {
		return
	}
	s, release, ok := h.lockSession(w, r, t, r.PathValue("id"))
	if !ok {
		if claim != nil {
			h.releaseIdem(r, claim, scope)
		}
		return
	}
	defer release()
	h.finish(w, r, t, s, h.updateSession(r, t, s, claim, scope, req, raw),
		func(w http.ResponseWriter, now *checkout.Session) bool {
			return h.refuseChange(w, r, now, updateAllow, "change")
		})
}

// updateSession is update once its session is loaded.
func (h *Handler) updateSession(
	r *http.Request, t *tenant.Tenant, s *checkout.Session,
	claim *checkout.Claim, scope string, req UpdateRequest, raw []byte,
) answer {
	if claim == nil {
		return fromSnapshot(func(w http.ResponseWriter) {
			h.renderReplay(w, r, t, s)
		})
	}
	// refused releases the key and answers with Django's refusal.
	refused := func(err error) answer {
		h.releaseIdem(r, claim, scope)
		return fromSnapshot(func(w http.ResponseWriter) {
			upstreamError(w, err)
		})
	}
	if !s.Open() {
		h.releaseIdem(r, claim, scope)
		return fromSnapshot(func(w http.ResponseWriter) {
			h.refuseChange(w, r, s, updateAllow, "change")
		})
	}

	// Before any cart write: a failed lookup must leave the cart and the
	// stored session agreeing. Nothing here is saved until the end.
	applyBuyer(s, req.Buyer, req.FulfillmentDetails)
	applyFulfillment(s, req.FulfillmentDetails,
		req.SelectedFulfillmentOptions)
	if err := h.applyRegion(r, t, s, req.FulfillmentDetails); err != nil {
		return refused(err)
	}
	hasCodes := req.Discounts != nil && req.Discounts.Codes != nil
	if req.LineItems != nil || hasCodes {
		// The lookups above can outlast the lock. A session another
		// request stored meanwhile, a completion included, gets none of
		// this update's cart writes; finish answers by what is stored.
		if err := h.store.Current(r.Context(), s); err != nil {
			h.releaseIdem(r, claim, scope)
			return fromSnapshot(func(w http.ResponseWriter) {
				saveFailed(w, err)
			})
		}
	}
	if req.LineItems != nil {
		if err := h.syncCart(r, t, s.CartID, req.LineItems); err != nil {
			return refused(err)
		}
	}
	if hasCodes {
		if err := checkout.ApplyDiscountCodes(
			r.Context(), h.dj, t, s, req.Discounts.Codes,
		); err != nil {
			return refused(err)
		}
	}
	if err := h.selectPayWay(r, t, s); err != nil {
		return refused(err)
	}
	s.Recompute()
	// The lookups above can outlast the lock, and another request may
	// have stored the session meanwhile, a completion included: this
	// snapshot is then not written over it.
	if err := h.store.Save(r.Context(), s); err != nil {
		h.releaseIdem(r, claim, scope)
		return fromSnapshot(func(w http.ResponseWriter) {
			saveFailed(w, err)
		})
	}
	_ = h.storeIdem(r, claim, scope, idemRecord{BodyHash: bodyHash(raw)})
	return afterSave(func(w http.ResponseWriter) { h.render(w, r, t, s) })
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
	var key string
	a := h.completeSession(r, t, s, req, raw, &key)
	if key != "" {
		w.Header().Set("Idempotency-Key", key)
	}
	h.finish(w, r, t, s, a,
		func(w http.ResponseWriter, now *checkout.Session) bool {
			return h.completeClosed(w, r, t, now)
		})
}

// completeClosed answers a complete of a session that is not open, and
// reports whether it did. One that placed its order is answered with
// the session: completed, or awaiting the payment its continue_url
// leads to (a session placed over UCP with a hosted pay way is driven
// here too). The order is what a complete asks for, and it exists.
func (h *Handler) completeClosed(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant,
	s *checkout.Session,
) bool {
	switch s.Status {
	case checkout.StatusCompleted, checkout.StatusRequiresEscalation:
		h.render(w, r, t, s)
	case checkout.StatusCompleteInProgress:
		h.writeCompletionPending(w, r, s)
	case checkout.StatusCanceled:
		writeClosed(w, "", "Session is canceled and cannot be completed.")
	default:
		return false
	}
	return true
}

// completeSession is complete once its session is loaded. echo is set to
// the idempotency key when the request holds it or replays it.
func (h *Handler) completeSession(
	r *http.Request, t *tenant.Tenant, s *checkout.Session,
	req CompleteRequest, raw []byte, echo *string,
) answer {
	// Decided by what the session is, before any lookup, and nothing is
	// written.
	if !s.Open() {
		return fromSnapshot(func(w http.ResponseWriter) {
			h.completeClosed(w, r, t, s)
		})
	}

	scope := "complete:" + s.ID
	key, claim, _, refuse := h.claimKey(r, t.SchemaName, scope, raw)
	if refuse != nil {
		return fromSnapshot(refuse)
	}
	*echo = key
	if claim == nil {
		// A replayed key answers the session as it is stored now: what
		// its first attempt made of it.
		return fromSnapshot(func(w http.ResponseWriter) {
			h.renderReplay(w, r, t, s)
		})
	}
	// refused releases the key: nothing was placed.
	refused := func(write func(http.ResponseWriter)) answer {
		h.releaseIdem(r, claim, scope)
		return fromSnapshot(write)
	}

	if req.PaymentData == nil {
		return refused(func(w http.ResponseWriter) {
			writeError(w, http.StatusBadRequest, Error{
				Type: "invalid_request", Code: "missing",
				Param: "$.payment_data", Message: "payment_data is required.",
			})
		})
	}
	// Delegated payment is not implemented: any payment handler or
	// tokenized instrument (Stripe SPT included) is refused. Card buyers
	// pay on the store's checkout via continue_url; only handler-less
	// completion (a collect-later pay way such as cash on delivery)
	// works here.
	if req.PaymentData.HandlerID != "" ||
		len(req.PaymentData.Instrument) > 0 {
		return refused(func(w http.ResponseWriter) {
			writeError(w, http.StatusBadRequest, Error{
				Type: "invalid_request", Code: "unsupported",
				Message: "Delegated card payment is not supported. To pay " +
					"by card, send the buyer to continue_url: the store's " +
					"checkout takes the card on its payment provider's " +
					"hosted page. To complete here, send empty " +
					"payment_data and the order is placed with the store's " +
					"pay-later method, such as cash on delivery.",
			})
		})
	}

	buyer, status := s.Buyer, s.Status
	applyBuyer(s, req.Buyer, nil)
	s.Recompute()
	// The buyer this call submitted is the latest word whether or not
	// the call places the order. It is stored now, because a refused
	// completion stores nothing: a later complete that omits the buyer
	// would otherwise place the order for the one before. The pay way is
	// not part of it: it is looked up again on every call.
	if s.Open() && (s.Buyer != buyer || s.Status != status) {
		if err := h.store.Save(r.Context(), s); err != nil {
			return refused(func(w http.ResponseWriter) { saveFailed(w, err) })
		}
	}
	// Readiness first, and it needs no pay way on ACP. Without a delivery no
	// pay way can be chosen, which says nothing about what the store offers.
	if s.Status == checkout.StatusIncomplete {
		return refused(func(w http.ResponseWriter) {
			h.completeError(w, r, s, checkout.ErrNotReady)
		})
	}
	if err := h.selectPayWay(r, t, s); err != nil {
		return refused(func(w http.ResponseWriter) { upstreamError(w, err) })
	}
	if s.PayWayID == 0 {
		return refused(func(w http.ResponseWriter) {
			writeError(w, http.StatusBadRequest, Error{
				Type: "invalid_request", Code: "unsupported",
				Message: "This store has no agent-completable payment " +
					"method; send the buyer to continue_url.",
			})
		})
	}

	placement, err := h.flow.Complete(r.Context(), t, s)
	if placement == checkout.NothingPlaced {
		// Refused before the order was sent, or by Django for the order
		// itself: either way nothing of this call exists.
		return refused(func(w http.ResponseWriter) {
			h.completeError(w, r, s, err)
		})
	}
	// Past the in-progress save, which only the stored session's own
	// request passes: what follows is this call's own outcome. A claim
	// that lapsed changes nothing in it, and the session says so to
	// whoever holds the key now.
	_ = h.storeIdem(r, claim, scope, idemRecord{BodyHash: bodyHash(raw)})
	return afterSave(func(w http.ResponseWriter) {
		if err != nil {
			h.completeError(w, r, s, err)
			return
		}
		if r.Context().Err() != nil {
			// The request's context has ended: the platform hung up, or the
			// server is shutting down. Reading the order back on it could
			// only fail. A platform still connected (a shutdown, or a
			// half-closed connection, which looks like a hang-up) reads the
			// answer, so it is the one a failed render gives.
			h.log.InfoContext(r.Context(),
				"acp: order placed, session not rendered",
				slog.String("session", s.ID),
				slog.String("order", s.OrderUUID),
				slog.String("cause", context.Cause(r.Context()).Error()))
			writePlacedUnrendered(w, s)
			return
		}
		h.render(w, r, t, s)
	})
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenantOf(w, r)
	if !ok {
		return
	}
	if !checkout.ValidID(r.PathValue("id")) {
		h.sessionError(w, checkout.ErrNotFound)
		return
	}
	scope := "cancel:" + r.PathValue("id")
	claim, _, handled := h.claimIdem(w, r, t.SchemaName, scope,
		[]byte(r.PathValue("id")))
	if handled {
		return
	}
	s, release, ok := h.lockSession(w, r, t, r.PathValue("id"))
	if !ok {
		if claim != nil {
			h.releaseIdem(r, claim, scope)
		}
		return
	}
	defer release()
	h.finish(w, r, t, s, h.cancelSession(r, t, s, claim, scope),
		func(w http.ResponseWriter, now *checkout.Session) bool {
			return h.refuseChange(w, r, now, "", "be canceled")
		})
}

// cancelSession is cancel once its session is loaded.
func (h *Handler) cancelSession(
	r *http.Request, t *tenant.Tenant, s *checkout.Session,
	claim *checkout.Claim, scope string,
) answer {
	if claim == nil {
		return fromSnapshot(func(w http.ResponseWriter) {
			h.renderReplay(w, r, t, s)
		})
	}
	// Once an order exists, or may, canceling the session would not
	// cancel the order: the buyer could still pay it, and the session
	// would read canceled over a paid order.
	if !s.Open() {
		h.releaseIdem(r, claim, scope)
		return fromSnapshot(func(w http.ResponseWriter) {
			h.refuseChange(w, r, s, "", "be canceled")
		})
	}
	s.Status = checkout.StatusCanceled
	if err := h.store.Save(r.Context(), s); err != nil {
		h.releaseIdem(r, claim, scope)
		return fromSnapshot(func(w http.ResponseWriter) {
			saveFailed(w, err)
		})
	}
	_ = h.storeIdem(r, claim, scope, idemRecord{
		BodyHash: bodyHash([]byte(r.PathValue("id"))),
	})
	return afterSave(func(w http.ResponseWriter) { h.render(w, r, t, s) })
}

// --- Shared plumbing -------------------------------------------------------

func (h *Handler) lockSession(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant, id string,
) (*checkout.Session, func(), bool) {
	release, err := h.store.Lock(r.Context(), t.SchemaName, id)
	if err != nil {
		if errors.Is(err, checkout.ErrLocked) {
			writeInFlight(w, idemRetryAfter, msgSessionBusy)
			return nil, nil, false
		}
		h.sessionError(w, err)
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

// msgSessionGone answers an id no session is stored for. The id does not
// say why: the session was never completed and expired, it placed an
// order and is no longer kept, or the id is wrong. So the answer never
// reads as "nothing happened, create another".
const msgSessionGone = "No checkout session is held for this id. It " +
	"may never have been completed, it may have placed an order and " +
	"no longer be kept, or the id may be wrong, so an order may " +
	"exist: check for it (the order id complete returned, or the " +
	"buyer's order confirmation) before creating another session."

func (h *Handler) sessionError(w http.ResponseWriter, err error) {
	if errors.Is(err, checkout.ErrNotFound) {
		writeError(w, http.StatusNotFound, Error{
			Type: "invalid_request", Code: "not_found",
			Message: msgSessionGone,
		})
		return
	}
	writeUnavailable(w, msgRecordsUnavailable)
}

// updateAllow is what a session that takes no change still serves: the
// GET route, which serves HEAD too.
const updateAllow = http.MethodGet + ", " + http.MethodHead

// refuseChange answers an update or a cancel (what names it: "change",
// "be canceled") of a session that takes neither, and reports whether it
// did: one whose order is being placed, and one that is canceled or may
// have an order (405, with what the target still serves).
func (h *Handler) refuseChange(
	w http.ResponseWriter, r *http.Request, s *checkout.Session,
	allow, what string,
) bool {
	switch {
	case s.Open():
		return false
	case s.Status == checkout.StatusCompleteInProgress:
		h.writeCompletionPending(w, r, s)
	case s.Status == checkout.StatusRequiresEscalation:
		writeClosed(w, allow, "Session has placed an order that awaits "+
			"its payment and cannot "+what+".")
	default:
		writeClosed(w, allow,
			fmt.Sprintf("Session is %s and cannot %s.", s.Status, what))
	}
	return true
}

// completeError answers a refused or failed completion of s. Every
// sentinel of checkout and of the Django client has its case here or in
// upstreamError (TestCompleteErrorMapsEverySentinel): a refusal is a 4xx,
// and an order that exists, or may, is never answered as a failure.
func (h *Handler) completeError(
	w http.ResponseWriter, r *http.Request, s *checkout.Session, err error,
) {
	var shortfall *django.StockShortfall
	switch {
	case errors.As(err, &shortfall):
		var lines []string
		for _, item := range shortfall.FailedItems {
			lines = append(lines, fmt.Sprintf(
				"%s: requested %d, available %d",
				item.ProductName, item.Requested, item.Available))
		}
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "out_of_stock",
			Message: "Not enough stock: " + strings.Join(lines, "; "),
		})
	case errors.Is(err, checkout.ErrNotFound):
		h.sessionError(w, err)
	case errors.Is(err, checkout.ErrLocked):
		writeInFlight(w, idemRetryAfter, msgSessionBusy)
	case errors.Is(err, checkout.ErrSessionChanged):
		saveFailed(w, err)
	case errors.Is(err, checkout.ErrNotReady):
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "missing",
			Message: "The session is missing buyer or fulfillment data.",
		})
	case errors.Is(err, checkout.ErrCompletionInProgress):
		h.writeCompletionPending(w, r, s)
	case errors.Is(err, checkout.ErrOrderOutcomeUnknown):
		writeOutcomeUnknown(w)
	case errors.Is(err, checkout.ErrPaymentSessionFailed):
		writeError(w, http.StatusBadGateway, Error{
			Type: "processing_error", Code: "payment_link_failed",
			Message: "Order " + s.OrderUUID + " is placed, but its " +
				"payment page could not be opened. Do not complete " +
				"again or start a new checkout.",
		})
	case errors.Is(err, checkout.ErrSessionNotSaved):
		writePlacedUnsaved(w, s.OrderUUID)
	// ErrCartHeld is a create's, and is answered as create answers it:
	// a cart made for a session is never another checkout's.
	case errors.Is(err, checkout.ErrUnavailable),
		errors.Is(err, checkout.ErrCartHeld):
		writeUnavailable(w, msgRecordsUnavailable)
	case errors.Is(err, checkout.ErrPayWayUnavailable),
		errors.Is(err, checkout.ErrPaymentMethodUnsupported):
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "unsupported",
			Message: "No agent-completable payment method serves this " +
				"delivery; send the buyer to continue_url.",
		})
	default:
		upstreamError(w, err)
	}
}

// render answers 200 with the session as it is now.
func (h *Handler) render(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant,
	s *checkout.Session,
) {
	if body, ok := h.renderBody(w, r, t, s); ok {
		writeBody(w, http.StatusOK, body)
	}
}

// renderReplay answers a replayed key of update, complete or cancel with
// the session as it is now.
func (h *Handler) renderReplay(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant,
	s *checkout.Session,
) {
	if body, ok := h.renderBody(w, r, t, s); ok {
		writeReplay(w, http.StatusOK, body)
	}
}

// renderBody encodes the session, or answers why it cannot and reports
// false.
func (h *Handler) renderBody(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant,
	s *checkout.Session,
) ([]byte, bool) {
	// ACP names no language: the store's default.
	payload, err := Render(r.Context(), h.dj, t,
		h.site.Links(t.Domain, t.DefaultLocale), s)
	if err == nil {
		var body []byte
		if body, err = json.Marshal(payload); err == nil {
			return body, true
		}
	}
	h.log.ErrorContext(r.Context(), "acp render failed",
		slog.String("session", s.ID), slog.String("error", err.Error()))
	switch {
	case s.OrderUUID != "":
		writePlacedUnrendered(w, s)
	case s.MayHaveOrder():
		// Being placed, or never confirmed: its cart may have gone with
		// the order, so it is answered as what it is, not as a session
		// to read again until it renders.
		h.writeCompletionPending(w, r, s)
	default:
		writeUnavailable(w, "The session cannot be rendered right now.")
	}
	return nil, false
}
