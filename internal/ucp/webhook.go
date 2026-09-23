package ucp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpsig"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
)

const (
	// Deliberately NOT per-schema: this is one work queue for the whole
	// pod pool and each delivery carries its own schema, which is what
	// the signing key is resolved from. Per-schema streams would need a
	// fair scheduler across an unbounded set of keys to gain anything the
	// worker pool below does not already give.
	eventsStream = "ag:webhooks:orders"
	eventsGroup  = "dispatch"
	// deliveryField is the stream entry's single field.
	deliveryField = "delivery"

	// deliveryWorkers bounds concurrent deliveries, so one blackholing
	// platform endpoint occupies one worker instead of the whole queue:
	// three attempts at a 15s client timeout plus 5s and 20s of backoff
	// is up to ~70s per event.
	deliveryWorkers = 8

	// visibilityTimeout is how long a delivery may stay unacknowledged
	// before another consumer takes it over. It must outlast the longest
	// delivery (~70s above), or a live pod's in-flight work would be
	// claimed and sent twice.
	visibilityTimeout = 3 * time.Minute
	// reclaimInterval paces the takeover of deliveries a dead consumer
	// left pending.
	reclaimInterval = 30 * time.Second
	// consumerExpiry removes consumers (pods long gone) that hold no
	// pending deliveries, so pod churn does not grow the group forever.
	consumerExpiry = time.Hour

	// drainTimeout bounds how long a shutdown waits for in-flight
	// deliveries; whatever is still unacknowledged stays pending for
	// another consumer.
	drainTimeout = 10 * time.Second
	// ackTimeout bounds the detached acknowledgement of a delivery so a
	// shutdown cannot hang on it.
	ackTimeout = 5 * time.Second
)

// signedComponents are the RFC 9421 components a webhook covers, in the
// order UCP's REST request signing lists them: @query only when the
// platform's URL carries one.
func signedComponents(withQuery bool) []string {
	c := []string{"@method", "@authority", "@path"}
	if withQuery {
		c = append(c, "@query")
	}
	return append(c, "ucp-agent", "content-digest", "content-type")
}

// Delivery is one queued order webhook. It is the queue record, never the
// wire body: Body is the order entity exactly as it will be signed and
// sent, rendered once at enqueue so every retry carries the same bytes
// under the same Webhook-Id.
type Delivery struct {
	// ID is the Webhook-Id platforms dedupe at-least-once delivery on.
	// Standard Webhooks requires it to stay the same across every retry
	// of one event, so the caller derives it from the event.
	ID string `json:"id"`
	// Schema selects the signing key; Domain names the business profile
	// in UCP-Agent.
	Schema    string          `json:"schema"`
	Domain    string          `json:"domain"`
	TargetURL string          `json:"targetUrl"`
	Body      json.RawMessage `json:"body"`
}

// Dispatcher delivers order webhooks from a Redis stream consumer group,
// at least once: an entry is acknowledged only on a terminal outcome
// (delivered, or permanently undeliverable), and one a consumer holds
// past visibilityTimeout — it crashed, or was shut down mid-delivery —
// is taken over by another. Each delivery is signed with its own
// tenant's key.
type Dispatcher struct {
	rdb      *redis.Client
	keys     *Keys
	hc       *http.Client
	log      *slog.Logger
	consumer string
}

// NewDispatcher builds a dispatcher consuming as consumer, which must be
// unique per process (the pod name). allowLocal is the ENV-driven
// AllowLocalWebhooks: only development and tests may deliver to
// loopback or private addresses.
func NewDispatcher(
	rdb *redis.Client, keys *Keys, log *slog.Logger,
	consumer string, allowLocal bool,
) *Dispatcher {
	return &Dispatcher{
		rdb:      rdb,
		keys:     keys,
		hc:       platformClient(allowLocal, 15*time.Second),
		log:      log,
		consumer: consumer,
	}
}

// Enqueue queues a delivery. Callers only acknowledge upstream (Django's
// Celery push) after this returns nil.
func (d *Dispatcher) Enqueue(ctx context.Context, dl Delivery) error {
	if dl.TargetURL == "" || dl.ID == "" {
		return errors.New("ucp: delivery without a target or id")
	}
	raw, err := json.Marshal(dl)
	if err != nil {
		return err
	}
	return d.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: eventsStream,
		Values: map[string]any{deliveryField: raw},
	}).Err()
}

// Run consumes the stream until ctx ends, then waits up to drainTimeout
// for in-flight deliveries. Start once per process and wait for it to
// return before closing Redis.
func (d *Dispatcher) Run(ctx context.Context) {
	if err := d.ensureGroup(ctx); err != nil {
		d.log.Error("webhook consumer group unavailable",
			slog.String("error", err.Error()))
	}

	// Deliveries outlive the consume loop by up to drainTimeout: a
	// shutdown lets a post in flight finish instead of abandoning it for
	// a duplicate from whichever consumer takes it over.
	deliveryCtx, cancelDeliveries := context.WithCancel(
		context.WithoutCancel(ctx))
	jobs := make(chan redis.XMessage)
	var wg sync.WaitGroup
	for range deliveryWorkers {
		wg.Go(func() {
			for msg := range jobs {
				d.deliver(deliveryCtx, msg)
			}
		})
	}
	defer func() {
		close(jobs)
		drain := time.AfterFunc(drainTimeout, cancelDeliveries)
		wg.Wait()
		drain.Stop()
		cancelDeliveries()
	}()

	// Replay this consumer's own unacknowledged entries first: a restart
	// under the same name (the same pod) resumes them at once instead of
	// waiting out visibilityTimeout for another consumer to take over.
	// Paged by id, since history reads return pending entries until
	// they are acknowledged.
	pendingFrom := "0"
	var lastReclaim time.Time
	for ctx.Err() == nil {
		var msgs []redis.XMessage
		switch {
		case pendingFrom != "":
			msgs = d.read(ctx, pendingFrom)
			if len(msgs) == 0 {
				pendingFrom = ""
				continue
			}
			pendingFrom = msgs[len(msgs)-1].ID
		case time.Since(lastReclaim) >= reclaimInterval:
			lastReclaim = time.Now()
			msgs = d.reclaim(ctx)
		}
		if len(msgs) == 0 && pendingFrom == "" {
			msgs = d.read(ctx, ">")
		}
		for _, msg := range msgs {
			select {
			case jobs <- msg:
			case <-ctx.Done():
				// Read but not started: it stays pending and is taken
				// over after visibilityTimeout.
				return
			}
		}
	}
}

func (d *Dispatcher) ensureGroup(ctx context.Context) error {
	err := d.rdb.XGroupCreateMkStream(ctx, eventsStream, eventsGroup, "0").
		Err()
	// BUSYGROUP: another pod (or an earlier boot) created it.
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// read fetches deliveries after id: ">" blocks for new ones, any other
// id pages this consumer's pending history. A missing group (the stream
// was deleted) is recreated; other failures back off.
func (d *Dispatcher) read(ctx context.Context, id string) []redis.XMessage {
	streams, err := d.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    eventsGroup,
		Consumer: d.consumer,
		Streams:  []string{eventsStream, id},
		Count:    deliveryWorkers,
		Block:    5 * time.Second,
	}).Result()
	switch {
	case err == nil:
		var msgs []redis.XMessage
		for _, s := range streams {
			msgs = append(msgs, s.Messages...)
		}
		return msgs
	case errors.Is(err, redis.Nil), ctx.Err() != nil:
		return nil
	}
	d.log.Warn("webhook queue read failed", slog.String("error", err.Error()))
	_ = d.ensureGroup(ctx)
	select {
	case <-time.After(time.Second):
	case <-ctx.Done():
	}
	return nil
}

// reclaim takes over deliveries another consumer has held past
// visibilityTimeout, and removes consumers that are long gone.
func (d *Dispatcher) reclaim(ctx context.Context) []redis.XMessage {
	msgs, _, err := d.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   eventsStream,
		Group:    eventsGroup,
		Consumer: d.consumer,
		MinIdle:  visibilityTimeout,
		Start:    "0-0",
		Count:    deliveryWorkers,
	}).Result()
	if err != nil && ctx.Err() == nil {
		d.log.Warn("webhook reclaim failed", slog.String("error", err.Error()))
	}

	consumers, err := d.rdb.XInfoConsumers(ctx, eventsStream, eventsGroup).
		Result()
	if err == nil {
		for _, c := range consumers {
			if c.Name != d.consumer && c.Pending == 0 &&
				c.Idle > consumerExpiry {
				_ = d.rdb.XGroupDelConsumer(ctx, eventsStream, eventsGroup,
					c.Name).Err()
			}
		}
	}
	return msgs
}

// deliver attempts one delivery and acknowledges it ONLY on a terminal
// outcome. A shutdown that aborts delivery first leaves the entry pending
// for another consumer — never acknowledged, which would drop an
// undelivered order webhook and break at-least-once.
func (d *Dispatcher) deliver(ctx context.Context, msg redis.XMessage) {
	raw, _ := msg.Values[deliveryField].(string)
	var dl Delivery
	if err := json.Unmarshal([]byte(raw), &dl); err != nil ||
		dl.Schema == "" || dl.TargetURL == "" {
		// Undeliverable as stored: acknowledge and drop.
		d.log.Error("webhook delivery corrupt", slog.String("entry", msg.ID))
		d.ack(msg.ID)
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
		key, err := d.keys.ForSchema(ctx, dl.Schema)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.log.Warn("webhook signing key unavailable",
				slog.String("schema", dl.Schema),
				slog.String("error", err.Error()))
			continue
		}
		if d.post(ctx, key, dl) {
			d.ack(msg.ID)
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
	// Retries exhausted against a live endpoint: give up so the entry
	// does not cycle forever. There is no dead-letter stream by design;
	// this permanent-failure log is the record.
	d.log.Error("webhook delivery failed permanently",
		slog.String("delivery", dl.ID),
		slog.String("schema", dl.Schema))
	d.ack(msg.ID)
}

// ack acknowledges and deletes a terminally handled entry. Its context is
// detached from delivery on purpose: when a successful post is followed
// immediately by shutdown, the delivery context is already cancelled and
// go-redis would skip the command, leaving a delivered entry to be sent
// again by whichever consumer takes it over.
func (d *Dispatcher) ack(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	if err := d.rdb.XAckDel(ctx, eventsStream, eventsGroup, "DELREF", id).
		Err(); err != nil {
		d.log.Warn("webhook ack failed; the delivery will repeat",
			slog.String("error", err.Error()))
	}
}

// post sends one signed delivery. Headers follow Standard Webhooks
// (Webhook-Id, Webhook-Timestamp); the signature is RFC 9421 over the
// UCP REST components, verifiable against the JWK in the tenant's
// profile, which UCP-Agent names.
func (d *Dispatcher) post(
	ctx context.Context, key *SigningKey, dl Delivery,
) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		dl.TargetURL, bytes.NewReader(dl.Body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Webhook-Id", dl.ID)
	// The attempt time, per Standard Webhooks: verifiers reject a stale
	// timestamp as a replay, and a delivery can be retried, backlogged or
	// taken over minutes after its event.
	now := time.Now()
	req.Header.Set("Webhook-Timestamp", strconv.FormatInt(now.Unix(), 10))
	req.Header.Set("UCP-Agent",
		`profile="`+storefront.UCPProfile(dl.Domain)+`"`)
	req.Header.Set("Content-Digest", httpsig.ContentDigest(dl.Body))

	if err := httpsig.Sign(req, "sig1",
		signedComponents(req.URL.RawQuery != ""),
		httpsig.Params{Created: now.Unix()}, key); err != nil {
		d.log.Error("webhook signing failed",
			slog.String("delivery", dl.ID), slog.String("error", err.Error()))
		return false
	}

	resp, err := d.hc.Do(req)
	if err != nil {
		d.log.Warn("webhook post failed",
			slog.String("delivery", dl.ID), slog.String("error", err.Error()))
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the keep-alive connection is reusable for the next
	// delivery to the same platform.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
