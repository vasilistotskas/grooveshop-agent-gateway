package server

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// orderEventBody is what Django's Celery task POSTs on order/payment
// status transitions.
type orderEventBody struct {
	SchemaName     string `json:"schemaName"`
	OrderUUID      string `json:"orderUuid"`
	Status         string `json:"status"`
	PaymentStatus  string `json:"paymentStatus"`
	TrackingNumber string `json:"trackingNumber"`
}

// internalOrderEvents handles POST /internal/events/order-status. The route
// is cluster-internal (never exposed via ingress); the shared secret is a
// second layer. Responding non-2xx makes Celery retry, which combined with
// the Redis-list dispatcher yields at-least-once platform delivery.
func internalOrderEvents(
	secret string,
	flow *checkout.Flow,
	dispatcher *ucp.Dispatcher,
	log *slog.Logger,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("X-Internal-Token")
		if subtle.ConstantTimeCompare(
			[]byte(provided), []byte(secret)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

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
				PermalinkURL: "https://" + session.Domain +
					"/checkout/success/" + body.OrderUUID,
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
	})
}
