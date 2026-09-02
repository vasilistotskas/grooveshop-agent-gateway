package server

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/feeds"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
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
// status transitions.
type orderEventBody struct {
	SchemaName     string `json:"schemaName"`
	OrderUUID      string `json:"orderUuid"`
	Status         string `json:"status"`
	PaymentStatus  string `json:"paymentStatus"`
	TrackingNumber string `json:"trackingNumber"`
}

// internalOrderEvents handles POST /internal/events/order-status.
// Responding non-2xx makes Celery retry, which combined with the
// Redis-list dispatcher yields at-least-once platform delivery.
func internalOrderEvents(
	secret string,
	flow *checkout.Flow,
	dispatcher *ucp.Dispatcher,
	log *slog.Logger,
) http.Handler {
	return requireInternalToken(secret, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		session, err := flow.ApplyOrderEvent(r.Context(),
			body.SchemaName, body.OrderUUID, body.PaymentStatus)
		if err != nil {
			log.ErrorContext(r.Context(), "order event apply failed",
				slog.String("order", body.OrderUUID),
				slog.String("error", err.Error()))
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		// Orders placed outside agent checkouts have no session — ACK so
		// Celery does not retry forever.
		if session == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if session.WebhookURL != "" {
			ev := ucp.OrderEvent{
				Schema:        body.SchemaName,
				CheckoutID:    session.ID,
				OrderUUID:     body.OrderUUID,
				Status:        body.Status,
				PaymentStatus: body.PaymentStatus,
				PermalinkURL: storefront.OrderSuccess(
					session.Domain, body.OrderUUID),
				OccurredAt: time.Now().UTC(),
				TargetURL:  session.WebhookURL,
			}
			if err := dispatcher.Enqueue(r.Context(), ev); err != nil {
				log.ErrorContext(r.Context(), "order event enqueue failed",
					slog.String("order", body.OrderUUID),
					slog.String("error", err.Error()))
				http.Error(w, "retry", http.StatusServiceUnavailable)
				return
			}
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
