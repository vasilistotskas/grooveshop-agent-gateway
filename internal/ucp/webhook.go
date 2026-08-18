package ucp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	eventsKey     = "ag:events:orders"
	processingKey = "ag:events:orders:processing"
)

// OrderEvent is one order-lifecycle update queued for platform delivery.
// Events carry a stable ID so platforms can dedupe at-least-once delivery.
type OrderEvent struct {
	ID            string    `json:"id"`
	Schema        string    `json:"schema"`
	CheckoutID    string    `json:"checkout_id"`
	OrderUUID     string    `json:"order_uuid"`
	Status        string    `json:"status"`
	PaymentStatus string    `json:"payment_status"`
	PermalinkURL  string    `json:"permalink_url"`
	OccurredAt    time.Time `json:"occurred_at"`

	WebhookURL string `json:"-"`
	// TargetURL rides the queued payload (the session's registered
	// platform endpoint at enqueue time).
	TargetURL string `json:"target_url"`
	Attempts  int    `json:"attempts"`
}

// Dispatcher delivers order webhooks from a Redis list with at-least-once
// semantics: enqueue is acknowledged only after LPUSH; a processing list
// covers crash windows between pop and delivery. Each event is signed
// with its own tenant's key.
type Dispatcher struct {
	rdb  *redis.Client
	keys *Keys
	hc   *http.Client
	log  *slog.Logger
	stop chan struct{}
}

func NewDispatcher(
	rdb *redis.Client, keys *Keys, log *slog.Logger,
) *Dispatcher {
	return &Dispatcher{
		rdb:  rdb,
		keys: keys,
		hc:   &http.Client{Timeout: 15 * time.Second},
		log:  log,
		stop: make(chan struct{}),
	}
}

// Enqueue queues an event for delivery. Callers only ACK upstream (Django's
// Celery push) after this returns nil.
func (d *Dispatcher) Enqueue(ctx context.Context, ev OrderEvent) error {
	if ev.TargetURL == "" {
		return nil // session registered no webhook — nothing to deliver
	}
	if ev.ID == "" {
		ev.ID = uuid.NewString()
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return d.rdb.LPush(ctx, eventsKey, raw).Err()
}

// Run consumes the queue until Stop. Start once per pod.
func (d *Dispatcher) Run(ctx context.Context) {
	// Reclaim events a crashed pod left in the processing list.
	d.reclaim(ctx)
	for {
		select {
		case <-d.stop:
			return
		case <-ctx.Done():
			return
		default:
		}
		raw, err := d.rdb.BLMove(ctx, eventsKey, processingKey,
			"RIGHT", "LEFT", 5*time.Second).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.log.Warn("webhook queue read failed",
				slog.String("error", err.Error()))
			time.Sleep(time.Second)
			continue
		}
		d.deliver(ctx, raw)
	}
}

func (d *Dispatcher) Stop() { close(d.stop) }

func (d *Dispatcher) reclaim(ctx context.Context) {
	for {
		raw, err := d.rdb.LMove(ctx, processingKey, eventsKey,
			"RIGHT", "LEFT").Result()
		if err != nil || raw == "" {
			return
		}
	}
}

func (d *Dispatcher) deliver(ctx context.Context, raw string) {
	defer func() {
		_ = d.rdb.LRem(ctx, processingKey, 1, raw).Err()
	}()

	var ev OrderEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		d.log.Error("webhook event corrupt", slog.String("error", err.Error()))
		return
	}
	if ev.Schema == "" {
		d.log.Error("webhook event missing schema", slog.String("event", ev.ID))
		return
	}

	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * 5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
		// The key lookup sits inside the retry loop so a Redis blip on a
		// cold cache gets the same backoff as a delivery failure.
		key, err := d.keys.ForSchema(ctx, ev.Schema)
		if err != nil {
			d.log.Warn("webhook signing key unavailable",
				slog.String("schema", ev.Schema),
				slog.String("error", err.Error()))
			continue
		}
		if d.post(ctx, key, ev.TargetURL, body) {
			return
		}
	}
	d.log.Error("webhook delivery failed permanently",
		slog.String("event", ev.ID),
		slog.String("order", ev.OrderUUID),
	)
}

// post signs the body with the tenant's Ed25519 key so platforms verify
// against the JWK published in that tenant's profile (kid header selects
// the key).
func (d *Dispatcher) post(
	ctx context.Context, key *SigningKey, url string, body []byte,
) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
		bytes.NewReader(body))
	if err != nil {
		return false
	}
	sig := ed25519.Sign(key.Private, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("UCP-Signature", base64.RawURLEncoding.EncodeToString(sig))
	req.Header.Set("UCP-Key-Id", key.KID)

	resp, err := d.hc.Do(req)
	if err != nil {
		d.log.Warn("webhook post failed",
			slog.String("url", url), slog.String("error", err.Error()))
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
