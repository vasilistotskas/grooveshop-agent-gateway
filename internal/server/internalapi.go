package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/feeds"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
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

// orderEventBudget bounds the whole answer to one order event: the
// reads, the session's update and the signed post to the platform.
// Django's outbox reads the answer for 30 s
// (AGENT_ORDER_EVENT_READ_TIMEOUT) and takes one it did not get for the
// gateway being unreachable, which stops its sweep of the store: the
// gateway answers first.
const orderEventBudget = 25 * time.Second

// Outcomes of gateway_order_webhooks_total.
const (
	webhookDelivered = "delivered"
	webhookFailed    = "failed"
)

// orderEventBody is one event of Django's outbox (django-api
// agent/outbox.py): an order an agent placed changed. Only the order is
// read from it: the webhook carries the order entity as Django reports
// it now, not the event's fields.
type orderEventBody struct {
	// EventID is the outbox row's id, and OccurredAt the event's time:
	// the Webhook-Id and the Webhook-Timestamp, the same on every attempt.
	EventID       string `json:"eventId"`
	OccurredAt    string `json:"occurredAt"`
	SchemaName    string `json:"schemaName"`
	OrderUUID     string `json:"orderUuid"`
	Status        string `json:"status"`
	PaymentStatus string `json:"paymentStatus"`
}

// orderEventDeps is what the order-event route drives.
type orderEventDeps struct {
	Store    *checkout.Store
	Flow     *checkout.Flow
	Resolver *tenant.Resolver
	Django   *django.Client
	Webhooks *ucp.Webhooks
	Site     storefront.Site
	Log      *slog.Logger
	Metrics  *obs.Metrics
}

// internalOrderEvents handles POST /internal/events/order-status.
//
// Django's outbox keeps the event and calls this until it settles. The
// gateway updates the checkout, and delivers the platform's webhook
// within the call, once, and its answer is the outcome: 204 delivered
// (or nothing to deliver), 503 try again later (with Retry-After when
// there is a time to name), 422 failed for good. A 400 is a body no
// retry fixes.
//
// Events route through the order link, not the checkout session: the
// session expires within a day, and the store keeps reporting shipment
// and delivery for weeks after.
func internalOrderEvents(secret string, d orderEventDeps) http.Handler {
	return requireInternalToken(secret, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), orderEventBudget)
		defer cancel()
		var body orderEventBody
		if err := json.NewDecoder(
			http.MaxBytesReader(w, r.Body, 64<<10),
		).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		eventID, eventErr := uuid.Parse(body.EventID)
		occurred, timeErr := time.Parse(time.RFC3339Nano, body.OccurredAt)
		if body.SchemaName == "" || body.OrderUUID == "" ||
			eventErr != nil || timeErr != nil {
			http.Error(w, "schemaName, orderUuid, eventId (a UUID) and "+
				"occurredAt (RFC 3339) are required", http.StatusBadRequest)
			return
		}
		obs.SetTenant(ctx, body.SchemaName)
		event := []any{
			slog.String("event", eventID.String()),
			slog.String("order", body.OrderUUID),
		}
		retry := func(msg string, err error) {
			d.Log.ErrorContext(ctx, "order event retried", append(event,
				slog.String("reason", msg),
				slog.String("error", err.Error()))...)
			http.Error(w, "retry", http.StatusServiceUnavailable)
		}
		fail := func(msg string, err error) {
			d.Log.ErrorContext(ctx, "order event failed", append(event,
				slog.String("reason", msg),
				slog.String("error", err.Error()))...)
			d.Metrics.OrderWebhooks.WithLabelValues(webhookFailed).Inc()
			http.Error(w, msg, http.StatusUnprocessableEntity)
		}

		link, err := d.Store.OrderLinkFor(ctx, body.SchemaName, body.OrderUUID)
		if errors.Is(err, checkout.ErrNotFound) {
			// Django sends an order's first event as the order commits,
			// and the completion that placed it writes the link once
			// Django answered: until that completion's budget has run out
			// from the event, the link may still come.
			wait := time.Until(occurred.Add(
				checkout.CompletionBudget(d.Django.Timeout())))
			if wait > 0 {
				w.Header().Set("Retry-After", retryAfterSeconds(wait))
				http.Error(w, "retry", http.StatusServiceUnavailable)
				return
			}
			// The link is lost (a flush or a restore, or a write that
			// failed): nothing is left to route the event through.
			d.Log.WarnContext(ctx, "order event without an order link",
				event...)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			retry("order link read failed", err)
			return
		}
		if err := d.Flow.ApplyOrderEvent(ctx, body.SchemaName,
			link.CheckoutID, body.OrderUUID, body.Status,
			body.PaymentStatus); err != nil {
			retry("order event apply failed", err)
			return
		}
		if link.WebhookURL == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		t, err := d.Resolver.Resolve(ctx, link.Domain)
		if errors.Is(err, tenant.ErrUnknownTenant) {
			fail("the store no longer resolves", err)
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
			fail("the store's domain moved to another store",
				fmt.Errorf("domain %s now resolves to %s",
					link.Domain, t.SchemaName))
			return
		}
		order, err := d.Django.PlacedOrder(
			ctx, t.Domain, t.DefaultLocale, body.OrderUUID)
		if errors.Is(err, django.ErrNotFound) {
			fail("the order no longer exists", err)
			return
		}
		if err != nil {
			retry("order event order fetch failed", err)
			return
		}
		// An order event names no language: the store's default.
		entity, err := ucp.BuildOrder(t,
			d.Site.Links(t.Domain, t.DefaultLocale), order, link.CheckoutID,
			ucp.OrderCapabilities())
		if err != nil {
			fail("the order cannot be rendered", err)
			return
		}
		raw, err := json.Marshal(entity)
		if err != nil {
			fail("the order cannot be encoded", err)
			return
		}
		reply, err := d.Webhooks.Send(ctx, ucp.Delivery{
			ID:         eventID.String(),
			OccurredAt: occurred,
			Schema:     t.SchemaName,
			Domain:     t.Domain,
			TargetURL:  link.WebhookURL,
			Body:       raw,
		})
		switch {
		case errors.Is(err, ucp.ErrPlatformURL):
			fail("the platform's webhook URL cannot be called", err)
		case err != nil:
			retry("order webhook unanswered", err)
		case reply.Delivered():
			d.Log.InfoContext(ctx, "order webhook delivered", append(event,
				slog.Int("status", reply.Status))...)
			d.Metrics.OrderWebhooks.WithLabelValues(webhookDelivered).Inc()
			w.WriteHeader(http.StatusNoContent)
		case reply.Retryable():
			d.Log.WarnContext(ctx, "order webhook to be retried", append(event,
				slog.Int("status", reply.Status))...)
			if reply.RetryAfter > 0 {
				w.Header().Set("Retry-After",
					retryAfterSeconds(reply.RetryAfter))
			}
			http.Error(w, "retry", http.StatusServiceUnavailable)
		default:
			fail("the platform refused the webhook",
				fmt.Errorf("platform answered %d", reply.Status))
		}
	}))
}

// retryAfterSeconds is a wait as Retry-After's delta-seconds, rounded up.
func retryAfterSeconds(wait time.Duration) string {
	return strconv.FormatInt(int64((wait+time.Second-1)/time.Second), 10)
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
		obs.SetTenant(r.Context(), body.SchemaName)

		removed, err := svc.Invalidate(r.Context(), body.SchemaName)
		if err != nil {
			log.ErrorContext(r.Context(), "feed invalidate failed",
				slog.String("error", err.Error()))
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"removed": removed})
	}))
}
