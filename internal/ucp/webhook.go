package ucp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// Deliberately NOT per-schema: this is one work queue for the whole
	// pod pool and each event carries its own schema, which is what the
	// signing key and the target URL are resolved from. Per-schema lists
	// would need a fair scheduler across an unbounded set of keys to
	// gain anything the worker pool below does not already give.
	eventsKey     = "ag:events:orders"
	processingKey = "ag:events:orders:processing"

	// deliveryWorkers bounds concurrent deliveries.
	//
	// Delivery used to run inline in the consumer loop, so ONE
	// undeliverable endpoint stalled every tenant's webhooks: three
	// attempts at a 15s client timeout plus 5s and 20s of backoff is up
	// to ~70s of head-of-line blocking per event, and a tenant whose
	// platform endpoint blackholes generates one such event per order
	// transition. Queued events for other tenants simply waited.
	//
	// A pool bounds the damage to one worker per stuck endpoint while
	// keeping at-least-once semantics unchanged: each event is still
	// moved to the processing list before delivery and removed only on a
	// terminal outcome (delivered, or permanently undeliverable). A
	// shutdown mid-delivery leaves the event on the processing list for
	// reclaim() to rescue on the next boot.
	deliveryWorkers = 8

	// ackTimeout bounds the detached removal of a terminally-handled event
	// so a shutdown cannot hang on it.
	ackTimeout = 5 * time.Second
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
// covers crash and shutdown windows between pop and a terminal outcome.
// Each event is signed with its own tenant's key.
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
//
// Reads are serial (one BLMove at a time keeps the processing-list
// bookkeeping simple); DELIVERY is handed to a bounded worker pool so a
// slow or dead platform endpoint occupies one worker instead of the
// whole queue.
func (d *Dispatcher) Run(ctx context.Context) {
	// Reclaim events a crashed pod left in the processing list.
	d.reclaim(ctx)

	jobs := make(chan string, deliveryWorkers)
	var wg sync.WaitGroup
	for range deliveryWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for raw := range jobs {
				d.deliver(ctx, raw)
			}
		}()
	}
	// Drain in-flight deliveries before returning so a shutdown does not
	// strand events in the processing list any longer than a crash would.
	defer func() {
		close(jobs)
		wg.Wait()
	}()

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
			select {
			case <-time.After(time.Second):
			case <-d.stop:
				return
			case <-ctx.Done():
				return
			}
			continue
		}
		select {
		case jobs <- raw:
		case <-d.stop:
			// Shutting down: hand the event straight back so it is not
			// lost between the pop and the (now closed) pool.
			_ = d.rdb.LMove(ctx, processingKey, eventsKey,
				"LEFT", "LEFT").Err()
			return
		case <-ctx.Done():
			return
		}
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

// deliver attempts one event and removes it from the processing list ONLY on
// a terminal outcome. The reliable-queue invariant is that an in-flight event
// stays on the processing list until it is acknowledged, so a shutdown that
// aborts delivery before a terminal outcome must leave the event there for
// reclaim() to requeue on the next boot — never remove it (that would drop an
// undelivered order webhook, violating at-least-once).
func (d *Dispatcher) deliver(ctx context.Context, raw string) {
	var ev OrderEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		// Unparseable: can never be delivered — acknowledge and drop.
		d.log.Error("webhook event corrupt", slog.String("error", err.Error()))
		d.ack(raw)
		return
	}
	if ev.Schema == "" {
		d.log.Error("webhook event missing schema", slog.String("event", ev.ID))
		d.ack(raw)
		return
	}

	body, err := json.Marshal(ev)
	if err != nil {
		d.log.Error("webhook event unencodable",
			slog.String("event", ev.ID), slog.String("error", err.Error()))
		d.ack(raw)
		return
	}
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * 5 * time.Second):
			case <-ctx.Done():
				return // shutting down: leave for reclaim(), do not ack
			}
		}
		// The key lookup sits inside the retry loop so a Redis blip on a
		// cold cache gets the same backoff as a delivery failure.
		key, err := d.keys.ForSchema(ctx, ev.Schema)
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down: leave for reclaim(), do not ack
			}
			d.log.Warn("webhook signing key unavailable",
				slog.String("schema", ev.Schema),
				slog.String("error", err.Error()))
			continue
		}
		if d.post(ctx, key, ev.TargetURL, body) {
			d.ack(raw) // delivered
			return
		}
		if ctx.Err() != nil {
			return // post aborted by shutdown: leave for reclaim(), do not ack
		}
	}
	// Retries exhausted against a live endpoint: give up and drop so the
	// event does not cycle forever. There is no dead-letter list by design;
	// this permanent-failure log is the record.
	d.log.Error("webhook delivery failed permanently",
		slog.String("event", ev.ID),
		slog.String("order", ev.OrderUUID),
	)
	d.ack(raw)
}

// ack removes a terminally-handled event from the processing list. It uses a
// context detached from delivery on purpose: when a successful post is
// immediately followed by shutdown, the delivery context is already
// cancelled and go-redis would skip the removal (it short-circuits commands
// on a done context), stranding a delivered event to be redelivered by
// reclaim(). A fresh, bounded context makes the acknowledgement fire anyway.
func (d *Dispatcher) ack(raw string) {
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	if err := d.rdb.LRem(ctx, processingKey, 1, raw).Err(); err != nil {
		d.log.Warn("webhook ack failed; event will redeliver on reclaim",
			slog.String("error", err.Error()))
	}
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
	// Drain so the keep-alive connection is reusable for the next
	// delivery to the same platform.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
