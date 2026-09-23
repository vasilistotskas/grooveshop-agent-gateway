package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/feeds"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// requireInternalToken gates the cluster-internal routes on the shared
// secret. The routes are never exposed via ingress; the token is a second
// layer, checked before the body is read so an unauthenticated caller
// cannot tell a malformed body from a valid one.
//
// The constant-time compare would accept an empty token against an empty
// secret, which is fine only because config.Load lists
// INTERNAL_EVENTS_SECRET as required — the gateway refuses to start
// without one, so these routes can never be live with an empty secret.
func requireInternalToken(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("X-Internal-Token")
		if subtle.ConstantTimeCompare(
			[]byte(provided), []byte(secret)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// orderEventBody is what Django's Celery task POSTs on order/payment
// status transitions. Only the order is read from it: the webhook carries
// the order entity as Django reports it now, not the event's fields.
type orderEventBody struct {
	SchemaName     string `json:"schemaName"`
	OrderUUID      string `json:"orderUuid"`
	Status         string `json:"status"`
	PaymentStatus  string `json:"paymentStatus"`
	TrackingNumber string `json:"trackingNumber"`
}

// webhookIDSpace namespaces event-derived Webhook-Ids (UUIDv5).
var webhookIDSpace = uuid.MustParse("8c6b8e0e-5f3a-4f1e-9a57-2f0d3b6c1a44")

// webhookID identifies one order event. Celery re-pushes an event whose
// acknowledgement was lost, and Standard Webhooks requires the id to stay
// the same across retries so the platform can dedupe, so it is derived
// from the event's own content rather than minted per push. Two pushes of
// the same state are the same event: the body is a state snapshot.
func (b orderEventBody) webhookID() string {
	return uuid.NewSHA1(webhookIDSpace, []byte(strings.Join([]string{
		b.SchemaName, b.OrderUUID, b.Status, b.PaymentStatus,
		b.TrackingNumber,
	}, "\x00"))).String()
}

// orderEventDeps is what the order-event route drives.
type orderEventDeps struct {
	Store      *checkout.Store
	Flow       *checkout.Flow
	Resolver   *tenant.Resolver
	Django     *django.Client
	Dispatcher *ucp.Dispatcher
	Log        *slog.Logger
}

// internalOrderEvents handles POST /internal/events/order-status.
//
// Events route through the order link, not the checkout session: the
// session expires within a day, and the store keeps reporting shipment
// and delivery for weeks after. Responding non-2xx makes Celery retry,
// which combined with the stream dispatcher yields at-least-once platform
// delivery; a 2xx is final, so it is reserved for events that are
// handled, or that no retry could ever deliver.
func internalOrderEvents(secret string, d orderEventDeps) http.Handler {
	return requireInternalToken(secret, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var body orderEventBody
		if err := json.NewDecoder(
			http.MaxBytesReader(w, r.Body, 64<<10),
		).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if body.SchemaName == "" || body.OrderUUID == "" {
			http.Error(w, "schemaName and orderUuid are required",
				http.StatusBadRequest)
			return
		}
		retry := func(msg string, err error) {
			d.Log.ErrorContext(ctx, msg,
				slog.String("order", body.OrderUUID),
				slog.String("error", err.Error()))
			http.Error(w, "retry", http.StatusServiceUnavailable)
		}
		drop := func(msg string, err error) {
			d.Log.ErrorContext(ctx, msg,
				slog.String("order", body.OrderUUID),
				slog.String("error", err.Error()))
			w.WriteHeader(http.StatusNoContent)
		}

		link, err := d.Store.OrderLinkFor(ctx, body.SchemaName, body.OrderUUID)
		if errors.Is(err, checkout.ErrNotFound) {
			// Placed outside an agent checkout: nothing to update.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			retry("order link read failed", err)
			return
		}
		if err := d.Flow.ApplyOrderEvent(ctx, body.SchemaName,
			link.CheckoutID, body.PaymentStatus); err != nil {
			retry("order event apply failed", err)
			return
		}
		if link.WebhookURL == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		t, err := d.Resolver.Resolve(ctx, link.Domain)
		if errors.Is(err, tenant.ErrUnknownTenant) {
			drop("order event for a store that no longer resolves", err)
			return
		}
		if err != nil {
			retry("order event tenant resolve failed", err)
			return
		}
		// The link names the domain; the event names the schema. A domain
		// that now belongs to another store must never receive this
		// store's order, signed with that store's key.
		if t.SchemaName != body.SchemaName {
			drop("order event domain moved to another store",
				fmt.Errorf("domain %s now resolves to %s",
					link.Domain, t.SchemaName))
			return
		}
		order, err := d.Django.OrderByUUID(
			ctx, t.Domain, t.DefaultLocale, body.OrderUUID)
		if errors.Is(err, django.ErrNotFound) {
			drop("order event for an order that no longer exists", err)
			return
		}
		if err != nil {
			retry("order event order fetch failed", err)
			return
		}
		entity, err := ucp.BuildOrder(t, order, link.CheckoutID,
			ucp.OrderCapabilities())
		if err != nil {
			drop("order event order unrenderable", err)
			return
		}
		raw, err := json.Marshal(entity)
		if err != nil {
			drop("order event order unencodable", err)
			return
		}
		if err := d.Dispatcher.Enqueue(ctx, ucp.Delivery{
			ID:        body.webhookID(),
			Schema:    t.SchemaName,
			Domain:    t.Domain,
			TargetURL: link.WebhookURL,
			Body:      raw,
		}); err != nil {
			retry("order event enqueue failed", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// feedInvalidateBody names the tenant whose feeds went stale.
type feedInvalidateBody struct {
	SchemaName string `json:"schemaName"`
}

// internalFeedInvalidate handles POST /internal/feeds/invalidate.
//
// The catalog feeds are cached in Redis for FEED_FRESH_TTL (6h) and the
// cache survives pod restarts, so before this endpoint the only way out
// of a stale feed was to wait it out or delete the keys by hand — a
// merchant's price change took up to six hours to reach Google, Meta and
// TikTok. Django's cache-purge service now calls this so a catalogue
// purge covers the feeds too. A non-2xx makes the caller retry.
func internalFeedInvalidate(
	secret string,
	svc *feeds.Service,
	log *slog.Logger,
) http.Handler {
	return requireInternalToken(secret, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body feedInvalidateBody
		if err := json.NewDecoder(
			http.MaxBytesReader(w, r.Body, 4<<10),
		).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if body.SchemaName == "" {
			http.Error(w, "schemaName is required", http.StatusBadRequest)
			return
		}

		removed, err := svc.Invalidate(r.Context(), body.SchemaName)
		if err != nil {
			log.ErrorContext(r.Context(), "feed invalidate failed",
				slog.String("schema", body.SchemaName),
				slog.String("error", err.Error()))
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"removed": removed})
	}))
}
