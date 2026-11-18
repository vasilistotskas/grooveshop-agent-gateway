package acp

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcptool"
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

// PathPrefix is where the ACP routes live: the server mounts
// WriteRouteError (httpmw.WithErrorWriter) on every path under it.
const PathPrefix = "/acp/"

// acpCaller is who reads a session here: ACP authenticates the store's
// own platform with its bearer, and binds no session to a signer.
var acpCaller = checkout.Caller{Protocol: checkout.ProtocolACP}

// WriteRouteError is the spec's Error for an answer written before the
// handler: the rate limiter, the tenant middleware, Recover, the mux's
// own 404 and 405. The status and message are the writer's, so the
// answer says no more about the store than the writer did.
func WriteRouteError(w http.ResponseWriter, status int, msg string) {
	e := Error{Type: "invalid_request", Code: "invalid", Message: msg}
	switch {
	case status == http.StatusNotFound:
		e.Code = "not_found"
	case status == http.StatusMethodNotAllowed:
		e.Code = "method_not_allowed"
	case status == http.StatusTooManyRequests:
		e.Type, e.Code = "service_unavailable", "rate_limited"
	case status == http.StatusServiceUnavailable:
		e.Type, e.Code = "service_unavailable", "unavailable"
	case status >= http.StatusInternalServerError:
		e.Type, e.Code = "processing_error", "internal_error"
	}
	writeError(w, status, e)
}

// msgRecordsUnavailable answers a checkout store (Redis) that could not
// be read or written: sessions, locks, idempotency keys. It names the
// gateway's own records, never the store's backend.
const msgRecordsUnavailable = "Checkout is temporarily unavailable."

// writeUnavailable is the protocol body for a transient gateway-side
// failure (Redis, rendering); the platform retries.
func writeUnavailable(w http.ResponseWriter, msg string) {
	w.Header().Set("Retry-After",
		strconv.Itoa(mcptool.UnavailableRetryAfter))
	writeError(w, http.StatusServiceUnavailable, Error{
		Type: "service_unavailable", Code: "unavailable", Message: msg,
	})
}

// writePlacedUnrendered answers for an order that exists while its
// session is not rendered: the platform must read it again, not order
// again.
func writePlacedUnrendered(w http.ResponseWriter, s *checkout.Session) {
	w.Header().Set("Retry-After",
		strconv.Itoa(mcptool.UnavailableRetryAfter))
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

// upstreamError maps Django client failures, and the line refusal
// cartLines makes before any of them, onto the protocol Error body.
func upstreamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, checkout.ErrInvalidQuantity):
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "invalid",
			Param: "$.line_items", Message: "Every line item's quantity " +
				"must be at least 1, and a product's lines together at " +
				"most 9007199254740991.",
		})
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
		w.Header().Set("Retry-After",
			strconv.Itoa(mcptool.UnavailableRetryAfter))
		writeError(w, http.StatusServiceUnavailable, Error{
			Type: "service_unavailable", Code: "upstream_unavailable",
			Message: "The store backend is temporarily unavailable.",
		})
	}
}

// maxBodyBytes bounds a request body.
const maxBodyBytes = 256 << 10

// decodeBody reads a POST's JSON body into out, and returns the body's
// fingerprint for its idempotency key.
func decodeBody(w http.ResponseWriter, r *http.Request, out any) (
	string, bool,
) {
	return readBody(w, r, out, false)
}

// decodeOptionalBody is decodeBody for a body the spec makes optional
// (cancel's): an empty one is accepted, and decodes nothing.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, out any) (
	string, bool,
) {
	return readBody(w, r, out, true)
}

func readBody(
	w http.ResponseWriter, r *http.Request, out any, optional bool,
) (string, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err == nil && optional && len(bytes.TrimSpace(raw)) == 0 {
		raw = nil
	} else if err == nil {
		err = json.Unmarshal(raw, out)
	}
	var hash string
	if err == nil {
		hash, err = fingerprint(raw)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "invalid",
			Message: "The request body is not valid JSON.",
		})
		return "", false
	}
	return hash, true
}

// --- Idempotency -----------------------------------------------------------

// idemRecord is what a settled key answered: the response that was sent,
// which every replay of the key answers byte for byte, whatever has
// become of the session since (spec §6.3: "the original response with
// the same HTTP status code").
type idemRecord struct {
	BodyHash string `json:"bodyHash"`
	Status   int    `json:"status"`
	Body     string `json:"body"`
	// Allow is the header a 405 was sent with, which may be empty.
	Allow *string `json:"allow,omitempty"`
}

const (
	// idemRecordTTL is the spec's minimum retention of a settled key.
	idemRecordTTL = 24 * time.Hour
	// idemVersion versions how every operation's key is recorded: bump it
	// when the record's shape or the body's fingerprint changes, so a
	// record an older pod wrote is a miss, never misread.
	idemVersion = "v3"
)

// The operations a key is scoped to.
const (
	opCreate   = "create"
	opUpdate   = "update"
	opComplete = "complete"
	opCancel   = "cancel"
)

// idemScope scopes a key to one endpoint: "create.v3", or an operation
// on one session ("complete.v3:<id>"). The spec scopes keys per endpoint,
// and a key reused on another session is a new request, not a replay of
// the first session's response.
func idemScope(op, id string) string {
	if id == "" {
		return op + "." + idemVersion
	}
	return op + "." + idemVersion + ":" + id
}

func idemRedisKey(schema, scope, key string) string {
	return "ag:" + schema + ":acp:idem:" + scope + ":" + key
}

// fingerprint identifies a body under the spec's request equivalence
// (§6.2: "semantic JSON equality of the request body only"). Decoded into
// plain values and encoded again, an object's members come out sorted
// and every number as a double, so key order and "1.0" against "1" make
// no difference, while null stays apart from an absent member and an
// array keeps its order. RFC 8785, which the spec suggests, compares
// numbers as doubles too. A body that is absent (nil) has a fingerprint
// of its own.
func fingerprint(raw []byte) (string, error) {
	canonical := raw
	if raw != nil {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", err
		}
		var err error
		if canonical, err = json.Marshal(v); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// maxIdempotencyKeyLen is the spec's bound on an Idempotency-Key:
// "Opaque string, max 255 characters. UUID v4 recommended." (the
// IdempotencyKey parameter, maxLength 255). Any string within it is a
// key; the bound keeps the Redis key built from it short.
const maxIdempotencyKeyLen = 255

// errKeyForm refuses an Idempotency-Key longer than the spec allows.
var errKeyForm = Error{
	Type: "invalid_request", Code: "invalid", Param: "Idempotency-Key",
	Message: fmt.Sprintf("The Idempotency-Key header must be 1 to %d "+
		"characters, such as a UUID.", maxIdempotencyKeyLen),
}

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

// keyed is a POST whose Idempotency-Key this request holds. Its answer is
// written to rec, and held there until the key is settled by it.
type keyed struct {
	claim *checkout.Claim
	scope string
	hash  string
	// mark names this request in the session it changes
	// (checkout.Session.AppliedBy): its endpoint, key and body.
	mark string
	rec  *heldResponse
	// orderUnsettled: see keptAgainstKey.
	orderUnsettled bool
}

// outcome notes what a body's answer says the request did.
func (k *keyed) outcome(a answer) {
	k.orderUnsettled = a.orderUnsettled
}

// claimKey implements POST idempotency, before anything else the request
// reads or writes: the first caller claims the key, a replay answers the
// stored response, a concurrent attempt gets 409, and a different body
// under the same key gets 422. It returns the key when this request holds
// it, and otherwise has answered.
func (h *Handler) claimKey(
	w http.ResponseWriter, r *http.Request, schema, scope, hash string,
) (*keyed, bool) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "idempotency_key_required",
			Message: "The Idempotency-Key header is required.",
		})
		return nil, false
	}
	if utf8.RuneCountInString(key) > maxIdempotencyKeyLen {
		writeError(w, http.StatusBadRequest, errKeyForm)
		return nil, false
	}
	claim, settled, err := h.store.Claim(r.Context(),
		idemRedisKey(schema, scope, key))
	if err != nil {
		writeUnavailable(w, msgRecordsUnavailable)
		return nil, false
	}
	if claim != nil {
		w.Header().Set("Idempotency-Key", key)
		return &keyed{
			claim: claim, scope: scope, hash: hash,
			mark: requestMark(scope, key, hash), rec: newHeldResponse(),
		}, true
	}
	// Held by another request, or settled with something that is not a
	// record: in flight either way.
	var rec idemRecord
	if settled == "" || json.Unmarshal([]byte(settled), &rec) != nil ||
		rec.Status == 0 {
		writeInFlight(w, idemRetryAfter, msgKeyInFlight)
		return nil, false
	}
	if rec.BodyHash != hash {
		writeError(w, http.StatusUnprocessableEntity, Error{
			Type: "invalid_request", Code: "idempotency_conflict",
			Message: "This Idempotency-Key was used with a different body.",
		})
		return nil, false
	}
	w.Header().Set("Idempotency-Key", key)
	if rec.Allow != nil {
		w.Header().Set("Allow", *rec.Allow)
	}
	w.Header().Set("Idempotent-Replayed", "true")
	writeBody(w, rec.Status, []byte(rec.Body))
	return nil, false
}

// requestMark is what a session keeps of the keyed request that changed
// it. The key is the platform's, so it is stored hashed.
func requestMark(scope, key, hash string) string {
	sum := sha256.Sum256([]byte(scope + "\n" + key + "\n" + hash))
	return hex.EncodeToString(sum[:])
}

// keptAgainstKey reports whether a key keeps the answer its request was
// given, which every replay then returns.
//
// Every answer that is final for the request is kept, a refusal
// included (400, 404, 405): "Same key + identical body → server
// MUST return the original response with the same HTTP status code"
// (§6.3). A platform that wants a new attempt after a refusal sends a
// new key. Freed, so that the same request can be sent again:
//   - a 5xx: "Responses with HTTP 5xx status codes MUST NOT be cached
//     against the idempotency key" (§6.5);
//   - a 409 idempotency_in_flight, the spec's answer to retry after
//     Retry-After (§6.4): the key or the session lock in use, or a
//     session another request changed meanwhile;
//   - a 429, a throttle, retried after its Retry-After too.
//
// Two 5xx answers of a completion are kept all the same, departing from
// §6.5: the order whose outcome the store never confirmed (502
// order_outcome_unknown), and the order placed whose session could not
// be saved (500 order_placed). An order may exist behind each, and the
// stored session may not say so, since the save that would have said it
// may be the one that failed: a retry run afresh would be answered by
// that session. So the key stays spent, and every retry of it is told
// what the first attempt was. The spec has no answer that says "an
// order may exist; do not place another" for a server to keep: a 4xx
// tells the client it erred, its 409 asks for a retry, and a complete's
// 200 must carry status completed and the order.
func keptAgainstKey(orderUnsettled bool, status int) bool {
	switch {
	case orderUnsettled:
		return true
	case status == http.StatusConflict,
		status == http.StatusTooManyRequests:
		return false
	}
	return status < http.StatusInternalServerError
}

// settle stores the held answer against its key, or frees the key for an
// answer that is not kept. It returns storeIdem's error.
func (h *Handler) settle(r *http.Request, k *keyed) error {
	status := k.rec.statusCode()
	if !keptAgainstKey(k.orderUnsettled, status) {
		h.releaseIdem(r, k.claim, k.scope)
		return nil
	}
	// A refusal only. The two 5xx answers a completion keeps say not to
	// complete again: an order may exist behind them.
	if status >= http.StatusBadRequest &&
		status < http.StatusInternalServerError {
		k.rec.noteKept()
	}
	rec := idemRecord{
		BodyHash: k.hash, Status: status, Body: k.rec.body.String(),
	}
	if allow, ok := k.rec.header["Allow"]; ok && len(allow) > 0 {
		rec.Allow = &allow[0]
	}
	return h.storeIdem(r, k.claim, k.scope, rec)
}

// settleAndSend settles the key by the held answer, then sends it. A key
// that could not be recorded is logged; the answer stands, since it
// says what the request did.
func (h *Handler) settleAndSend(
	w http.ResponseWriter, r *http.Request, k *keyed,
) {
	_ = h.settle(r, k)
	k.rec.send(w)
}

// heldResponse is an answer kept from the wire until its key is settled,
// so that what a key stores is what was sent.
type heldResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newHeldResponse() *heldResponse {
	return &heldResponse{header: http.Header{}}
}

func (a *heldResponse) Header() http.Header { return a.header }

func (a *heldResponse) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
}

func (a *heldResponse) Write(p []byte) (int, error) {
	a.WriteHeader(http.StatusOK)
	return a.body.Write(p)
}

// msgRefusalKept ends the message of a refusal its key keeps. Its
// message may name a condition that changes (stock, an unpaid order,
// missing data), and the same request sent again with the same key is
// answered this refusal whatever has changed since.
const msgRefusalKept = " This Idempotency-Key keeps this answer for " +
	"24 hours: send a new attempt with a new key."

// noteKept adds msgRefusalKept to the Error the answer holds, before the
// key stores it, so the first answer and every replay say it.
func (a *heldResponse) noteKept() {
	var e Error
	if json.Unmarshal(a.body.Bytes(), &e) != nil || e.Message == "" {
		return
	}
	e.Message += msgRefusalKept
	a.body.Reset()
	_ = json.NewEncoder(&a.body).Encode(e)
}

// statusCode is the status sent: net/http's 200 for an answer that wrote
// none.
func (a *heldResponse) statusCode() int {
	return cmp.Or(a.status, http.StatusOK)
}

func (a *heldResponse) send(w http.ResponseWriter) {
	maps.Copy(w.Header(), a.header)
	w.WriteHeader(a.statusCode())
	_, _ = w.Write(a.body.Bytes())
}

// settling is the context a claimed key is recorded or released on
// (checkout.Store.Settling). It outlives the request's own (a platform
// that hung up, a shutdown): a key left in flight answers the retry 409
// until its claim lapses.
func (h *Handler) settling(r *http.Request) (
	context.Context, context.CancelFunc,
) {
	return h.store.Settling(r.Context())
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
		// follows it buys one. One it does send must be a count
		// (checkout.MergeLines).
		qty := 1
		if ref.Quantity != nil {
			qty = *ref.Quantity
		}
		lines = append(lines,
			checkout.Line{ProductID: productID, Quantity: qty})
	}
	return checkout.MergeLines(lines)
}

func (h *Handler) syncCart(
	r *http.Request, t *tenant.Tenant, cartID string, lines []checkout.Line,
) error {
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
	hash, ok := decodeBody(w, r, &req)
	if !ok {
		return
	}
	k, ok := h.claimKey(w, r, t.SchemaName, idemScope(opCreate, ""), hash)
	if !ok {
		return
	}
	s := h.createSession(k.rec, r, t, req)
	err := h.settle(r, k)
	switch {
	case err == nil, s == nil:
		k.rec.send(w)
	case !errors.Is(err, errClaimLapsed):
		// Whether the key holds this response is not known, so the
		// session is not handed out: answered 201 without its record, a
		// retry of the key would create a second one. It is not
		// discarded either, since the record may name it. The retry
		// replays it if it was recorded, and creates anew if not.
		writeUnavailable(w, msgRecordsUnavailable)
	default:
		// The key is another create's by now, and one key answers one
		// session: this one was never handed out, and is not kept.
		h.discard(r, s)
		writeInFlight(w, idemRetryAfter, msgKeyInFlight)
	}
}

// createSession is create once its key is held. It answers into w, and
// returns the session it stored, which it answered 201 with, or nil.
func (h *Handler) createSession(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant,
	req CreateRequest,
) *checkout.Session {
	if len(req.LineItems) == 0 {
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "missing",
			Param: "$.line_items", Message: "line_items must not be empty.",
		})
		return nil
	}
	// Refused, not converted: amounts the platform reads as its own
	// currency would be the store's.
	if !strings.EqualFold(req.Currency, t.DefaultCurrency) {
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "invalid", Param: "$.currency",
			Message: "This store sells in " + t.DefaultCurrency + ".",
		})
		return nil
	}

	// Every lookup and refusal before the first cart write, which here is
	// creating the cart: an item, a region or a pay way the store refuses
	// leaves no cart behind. A line sync failing after it does.
	refused := func(err error) *checkout.Session {
		upstreamError(w, err)
		return nil
	}
	lines, err := cartLines(req.LineItems)
	if err != nil {
		return refused(err)
	}
	s := checkout.NewSession(t.SchemaName, t.Domain, checkout.ProtocolACP, "")
	applyBuyer(s, req.Buyer, req.FulfillmentDetails)
	applyFulfillment(s, req.FulfillmentDetails, nil)
	if err := h.applyRegion(r, t, s, req.FulfillmentDetails); err != nil {
		return refused(err)
	}
	if err := h.selectPayWay(r, t, s); err != nil {
		return refused(err)
	}

	cart, err := h.dj.GetCart(r.Context(), t.Domain, t.DefaultLocale, "")
	if err != nil {
		return refused(err)
	}
	s.CartID = cart.UUID
	if err := h.syncCart(r, t, s.CartID, lines); err != nil {
		return refused(err)
	}
	// Discounts apply after the lines exist (eligibility can depend on
	// the cart subtotal). A refused code is not a request failure — it
	// renders in discounts.rejected.
	if req.Discounts != nil && req.Discounts.Codes != nil {
		if err := checkout.ApplyDiscountCodes(
			r.Context(), h.dj, t, s, req.Discounts.Codes,
		); err != nil {
			return refused(err)
		}
	}
	s.Recompute()
	// Rendered before the save, which it does not read: a session that
	// cannot be answered is not stored, and a 5xx is never stored against
	// the key, so a retry is a new create that leaves nothing behind.
	body, ok := h.renderBody(w, r, t, s)
	if !ok {
		return nil
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
		writeUnavailable(w, msgRecordsUnavailable)
		return nil
	}
	writeBody(w, http.StatusCreated, body)
	return s
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
	s, err := h.store.Load(r.Context(), t.SchemaName, acpCaller,
		r.PathValue("id"))
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
	// orderUnsettled: a completion whose order may exist while the stored
	// session may not say so (see keptAgainstKey).
	orderUnsettled bool
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
//
// A stored session that holds this request's mark (an update's or a
// cancel's; a complete passes none) is its own save, whose reply Redis
// lost: the change is made, and the session is the answer, as it is to
// the request's retry (alreadyApplied).
func (h *Handler) finish(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant,
	s *checkout.Session, a answer, mark string,
	stored func(http.ResponseWriter, *checkout.Session) bool,
) {
	if a.current {
		a.write(w)
		return
	}
	// The body may be answering a caller that left; the read is short
	// and its answer goes nowhere then.
	now, err := h.store.Load(r.Context(), t.SchemaName, acpCaller,
		s.ID)
	switch {
	case err != nil:
		h.sessionError(w, err)
	case now.Revision == s.Revision:
		a.write(w)
	case mark != "" && now.AppliedBy == mark:
		h.render(w, r, t, now)
	case !stored(w, now):
		saveFailed(w, checkout.ErrSessionChanged)
	}
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenantOf(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !checkout.ValidID(id) {
		h.sessionError(w, checkout.ErrNotFound)
		return
	}
	var req UpdateRequest
	hash, ok := decodeBody(w, r, &req)
	if !ok {
		return
	}
	k, ok := h.claimKey(w, r, t.SchemaName, idemScope(opUpdate, id), hash)
	if !ok {
		return
	}
	if s, release, ok := h.lockSession(k.rec, r, t, id); ok {
		defer release()
		a := h.updateSession(r, t, s, req, k.mark)
		h.finish(k.rec, r, t, s, a, k.mark,
			func(w http.ResponseWriter, now *checkout.Session) bool {
				return h.refuseChange(w, r, now, updateAllow, "change")
			})
		k.outcome(a)
	}
	h.settleAndSend(w, r, k)
}

// updateSession is update once its session is loaded.
func (h *Handler) updateSession(
	r *http.Request, t *tenant.Tenant, s *checkout.Session, req UpdateRequest,
	mark string,
) answer {
	if s.AppliedBy == mark {
		return h.alreadyApplied(r, t, s)
	}
	// refused answers with Django's refusal.
	refused := func(err error) answer {
		return fromSnapshot(func(w http.ResponseWriter) {
			upstreamError(w, err)
		})
	}
	if !s.Open() {
		return fromSnapshot(func(w http.ResponseWriter) {
			h.refuseChange(w, r, s, updateAllow, "change")
		})
	}

	// Every lookup and refusal before any cart write: one that fails must
	// leave the cart and the stored session agreeing. Nothing here is
	// saved until the end.
	var lines []checkout.Line
	if req.LineItems != nil {
		var err error
		if lines, err = cartLines(req.LineItems); err != nil {
			return refused(err)
		}
	}
	applyBuyer(s, req.Buyer, req.FulfillmentDetails)
	applyFulfillment(s, req.FulfillmentDetails,
		req.SelectedFulfillmentOptions)
	if err := h.applyRegion(r, t, s, req.FulfillmentDetails); err != nil {
		return refused(err)
	}
	if err := h.selectPayWay(r, t, s); err != nil {
		return refused(err)
	}
	hasCodes := req.Discounts != nil && req.Discounts.Codes != nil
	if req.LineItems != nil || hasCodes {
		// The lookups above can outlast the lock. A session another
		// request stored meanwhile, a completion included, gets none of
		// this update's cart writes; finish answers by what is stored.
		if err := h.store.Current(r.Context(), s); err != nil {
			return fromSnapshot(func(w http.ResponseWriter) {
				saveFailed(w, err)
			})
		}
	}
	if req.LineItems != nil {
		if err := h.syncCart(r, t, s.CartID, lines); err != nil {
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
	s.Recompute()
	s.AppliedBy = mark
	// The lookups above can outlast the lock, and another request may
	// have stored the session meanwhile, a completion included: this
	// snapshot is then not written over it.
	if err := h.store.Save(r.Context(), s); err != nil {
		return fromSnapshot(func(w http.ResponseWriter) {
			saveFailed(w, err)
		})
	}
	return afterSave(func(w http.ResponseWriter) { h.render(w, r, t, s) })
}

func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenantOf(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !checkout.ValidID(id) {
		h.sessionError(w, checkout.ErrNotFound)
		return
	}
	var req CompleteRequest
	hash, ok := decodeBody(w, r, &req)
	if !ok {
		return
	}
	// The key first, whatever the session is: a replay of a completion is
	// answered what that completion was, and a complete without a key is
	// refused on every session.
	k, ok := h.claimKey(w, r, t.SchemaName, idemScope(opComplete, id), hash)
	if !ok {
		return
	}
	if s, release, ok := h.lockSession(k.rec, r, t, id); ok {
		defer release()
		a := h.completeSession(r, t, s, req)
		h.finish(k.rec, r, t, s, a, "",
			func(w http.ResponseWriter, now *checkout.Session) bool {
				return h.completeClosed(w, r, t, now)
			})
		k.outcome(a)
	}
	h.settleAndSend(w, r, k)
}

// completeClosed answers a complete of a session that is not open, and
// reports whether it did. One that placed its order is answered with
// the completed session: the order is what a complete asks for, and it
// exists. An ACP session never awaits a payment: it completes only with
// a collect-later pay way (checkout.Flow.Complete).
func (h *Handler) completeClosed(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant,
	s *checkout.Session,
) bool {
	switch {
	case s.Status == checkout.StatusCompleteInProgress:
		h.writeCompletionPending(w, r, s)
	case s.Status == checkout.StatusCanceled:
		writeClosed(w, "", "Session is canceled and cannot be completed.")
	case s.MayHaveOrder():
		// Its order is placed: the session is the answer. Every session
		// that is not open is answered here, so no complete of one is
		// left without an answer.
		h.render(w, r, t, s)
	default:
		return false
	}
	return true
}

// completeSession is complete once its session is loaded.
func (h *Handler) completeSession(
	r *http.Request, t *tenant.Tenant, s *checkout.Session,
	req CompleteRequest,
) answer {
	// Decided by what the session is, before any lookup, and nothing is
	// written.
	if !s.Open() {
		return fromSnapshot(func(w http.ResponseWriter) {
			h.completeClosed(w, r, t, s)
		})
	}

	if req.PaymentData == nil {
		return fromSnapshot(func(w http.ResponseWriter) {
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
		return fromSnapshot(func(w http.ResponseWriter) {
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
			return fromSnapshot(func(w http.ResponseWriter) { saveFailed(w, err) })
		}
	}
	// Readiness first, and it needs no pay way on ACP. Without a delivery no
	// pay way can be chosen, which says nothing about what the store offers.
	if s.Status == checkout.StatusIncomplete {
		return fromSnapshot(func(w http.ResponseWriter) {
			h.completeError(w, r, s, checkout.ErrNotReady)
		})
	}
	if err := h.selectPayWay(r, t, s); err != nil {
		return fromSnapshot(func(w http.ResponseWriter) { upstreamError(w, err) })
	}
	if s.PayWayID == 0 {
		return fromSnapshot(func(w http.ResponseWriter) {
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
		return fromSnapshot(func(w http.ResponseWriter) {
			h.completeError(w, r, s, err)
		})
	}
	// Past the in-progress save, which only the stored session's own
	// request passes: what follows is this call's own outcome. A claim
	// that lapsed changes nothing in it, and the session says so to
	// whoever holds the key now.
	a := afterSave(func(w http.ResponseWriter) {
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
	a.orderUnsettled = errors.Is(err, checkout.ErrOrderOutcomeUnknown) ||
		errors.Is(err, checkout.ErrSessionNotSaved)
	return a
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenantOf(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !checkout.ValidID(id) {
		h.sessionError(w, checkout.ErrNotFound)
		return
	}
	// The body is optional (CancelSessionRequest) and read only for the
	// key: nothing in it changes what a cancel does.
	hash, ok := decodeOptionalBody(w, r, &struct{}{})
	if !ok {
		return
	}
	k, ok := h.claimKey(w, r, t.SchemaName, idemScope(opCancel, id), hash)
	if !ok {
		return
	}
	if s, release, ok := h.lockSession(k.rec, r, t, id); ok {
		defer release()
		a := h.cancelSession(r, t, s, k.mark)
		h.finish(k.rec, r, t, s, a, k.mark,
			func(w http.ResponseWriter, now *checkout.Session) bool {
				return h.refuseChange(w, r, now, "", "be canceled")
			})
		k.outcome(a)
	}
	h.settleAndSend(w, r, k)
}

// cancelSession is cancel once its session is loaded.
func (h *Handler) cancelSession(
	r *http.Request, t *tenant.Tenant, s *checkout.Session, mark string,
) answer {
	if s.AppliedBy == mark {
		return h.alreadyApplied(r, t, s)
	}
	// Once an order exists, or may, canceling the session would not
	// cancel the order: the buyer could still pay it, and the session
	// would read canceled over a paid order.
	if !s.Open() {
		return fromSnapshot(func(w http.ResponseWriter) {
			h.refuseChange(w, r, s, "", "be canceled")
		})
	}
	s.Status = checkout.StatusCanceled
	s.AppliedBy = mark
	if err := h.store.Save(r.Context(), s); err != nil {
		return fromSnapshot(func(w http.ResponseWriter) {
			saveFailed(w, err)
		})
	}
	return afterSave(func(w http.ResponseWriter) { h.render(w, r, t, s) })
}

// alreadyApplied answers an update or a cancel whose change the session
// already holds: this request's first attempt was saved, and then could
// not answer (a render that failed frees the key, a pod that died never
// settled it). Run again, a cancel would be refused as a cancel of a
// canceled session, and that 405 kept against the key. So the retry
// changes nothing and is answered with the session, as the first attempt
// would have been.
func (h *Handler) alreadyApplied(
	r *http.Request, t *tenant.Tenant, s *checkout.Session,
) answer {
	return fromSnapshot(func(w http.ResponseWriter) { h.render(w, r, t, s) })
}

// --- Shared plumbing -------------------------------------------------------

func (h *Handler) lockSession(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant, id string,
) (*checkout.Session, func(), bool) {
	s, release, err := h.store.LockSession(r.Context(), t.SchemaName,
		acpCaller, id)
	switch {
	case errors.Is(err, checkout.ErrLocked):
		writeInFlight(w, idemRetryAfter, msgSessionBusy)
		return nil, nil, false
	case err != nil:
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
// did: one whose order is being placed, and one that is canceled or
// completed (405, with what the target still serves).
func (h *Handler) refuseChange(
	w http.ResponseWriter, r *http.Request, s *checkout.Session,
	allow, what string,
) bool {
	switch {
	case s.Open():
		return false
	case s.Status == checkout.StatusCompleteInProgress:
		h.writeCompletionPending(w, r, s)
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
	case errors.Is(err, checkout.ErrChoiceRefused):
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "missing",
			Message: "The store refuses one of this session's choices " +
				"for its cart now. Read the session, which names it, " +
				"change it with an update, then complete with a new " +
				"Idempotency-Key.",
		})
	case errors.Is(err, checkout.ErrTotalNotShown):
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "missing",
			Message: "The session has shown no total yet. Read it, " +
				"confirm the total with the buyer, then complete with a " +
				"new Idempotency-Key.",
		})
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
	case errors.Is(err, checkout.ErrPaymentWindowClosed),
		errors.Is(err, checkout.ErrPaymentAwaitsStore):
		// No ACP order awaits an online payment (Flow.admit), so no
		// completion here opens a link; answered for what it says.
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "order_not_payable",
			Message: "Order " + s.OrderUUID + " is placed and can no " +
				"longer be paid. Do not complete again or start a new " +
				"checkout before checking the order.",
		})
	case errors.Is(err, checkout.ErrSessionNotSaved):
		writePlacedUnsaved(w, s.OrderUUID)
	// ErrCartHeld is a create's, and is answered as create answers it:
	// a cart made for a session is never another checkout's.
	case errors.Is(err, checkout.ErrUnavailable),
		errors.Is(err, checkout.ErrCartHeld):
		writeUnavailable(w, msgRecordsUnavailable)
	// ErrInvalidIdempotencyKey is UCP's completion claim's, and is
	// answered as claimKey answers a header out of its bound.
	case errors.Is(err, checkout.ErrInvalidIdempotencyKey):
		writeError(w, http.StatusBadRequest, errKeyForm)
	case errors.Is(err, checkout.ErrPayWayUnavailable),
		errors.Is(err, checkout.ErrPaymentMethodUnsupported):
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "unsupported",
			Message: "No agent-completable payment method serves this " +
				"delivery; send the buyer to continue_url.",
		})
	// The session stays ready, and its next read shows, and stores, the
	// total now, which a complete under a new key places the order at.
	case errors.Is(err, django.ErrTotalChanged):
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: django.CodeTotalChanged,
			Message: "The store's total for this session changed since it " +
				"was shown, so no order was placed. Read the session, " +
				"confirm the new total with the buyer, then complete again.",
		})
	// Django names no order, and neither does the answer. The session
	// stays ready: once that order is canceled unpaid it can be completed.
	case errors.Is(err, django.ErrCartHasUnpaidOrder):
		writeError(w, http.StatusBadRequest, Error{
			Type: "invalid_request", Code: "cart_has_unpaid_order",
			Message: "An order placed from this session's cart already " +
				"awaits its payment, so this session placed no order. Do " +
				"not create another session for this cart. If the buyer " +
				"pays that order, it holds these goods and this session " +
				"is not needed; if the store cancels it unpaid when its " +
				"payment window ends, this session can then be completed.",
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

// storeShown stores the total a render of a stored session showed, which
// its order is placed at: a session publishes only what is stored, so
// one that cannot be stored is not rendered (renderBody). A session not
// created yet is stored by its create, with the total.
//
// A read (GET, HEAD) holds no lock, and stores it only by taking the
// lock (checkout.Store.StoreShown): the update or complete holding it
// never loses its save to a read.
func (h *Handler) storeShown(r *http.Request, s *checkout.Session) error {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return h.store.StoreShown(r.Context(), acpCaller, s)
	}
	err := h.store.Save(r.Context(), s)
	if err != nil {
		h.log.WarnContext(r.Context(), "acp: shown total not stored",
			slog.String("session", s.ID), slog.String("error", err.Error()))
	}
	return err
}

// renderBody encodes the session, or answers why it cannot and reports
// false.
func (h *Handler) renderBody(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant,
	s *checkout.Session,
) ([]byte, bool) {
	// ACP names no language: the store's default.
	shown := s.ShownTotal
	payload, err := Render(r.Context(), h.dj, t,
		h.site.Links(t.Domain, t.DefaultLocale), s)
	if err == nil && s.ShownTotal != shown && s.Revision != "" {
		// A total that is not stored is not published: its order would
		// be placed at the one stored before.
		if err := h.storeShown(r, s); err != nil {
			switch {
			case errors.Is(err, checkout.ErrLocked):
				writeInFlight(w, idemRetryAfter, msgSessionBusy)
			case errors.Is(err, checkout.ErrSessionChanged):
				saveFailed(w, err)
			case errors.Is(err, checkout.ErrNotFound):
				h.sessionError(w, err)
			default:
				writeUnavailable(w, msgRecordsUnavailable)
			}
			return nil, false
		}
	}
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
