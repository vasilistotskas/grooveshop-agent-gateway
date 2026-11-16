package ucp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpsig"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
)

const (
	// Deliberately NOT per-schema: this is one work queue for the whole
	// pod pool and each delivery carries its own schema, which is what
	// the signing key is resolved from. Per-schema streams would need a
	// fair scheduler across an unbounded set of keys; what one slow
	// endpoint can hold up is bounded by perTargetDeliveries instead.
	eventsStream = "ag:webhooks:orders"
	eventsGroup  = "dispatch"
	// deliveryField is the stream entry's single field.
	deliveryField = "delivery"

	// deliveryWorkers bounds concurrent deliveries per pod.
	deliveryWorkers = 8
	// postTimeout bounds one attempt of a delivery.
	postTimeout = 15 * time.Second
	// deliveryBudget is the most a delivery may take: three attempts, 5s
	// and 20s of backoff between them, and 10s for loading the signing
	// key. A delivery still running at its end is cut and left pending.
	deliveryBudget = 3*postTimeout + 5*time.Second + 20*time.Second +
		10*time.Second

	// perTargetDeliveries caps the deliveries one target (deliveryTarget)
	// holds at once on a pod, so an endpoint that does not answer
	// occupies at most this many of the deliveryWorkers, whatever number
	// of its events is queued, and the others keep delivering for every
	// other platform.
	perTargetDeliveries = 2
	// A delivery read while its target is at the cap waits in process, in
	// its target's queue, and the worker that frees one of the target's
	// slots takes it next: a burst to a platform goes out as fast as the
	// platform answers. A queue holds at most maxQueuedPerTarget, all
	// queues together maxQueued; a delivery beyond either is left
	// pending, never acknowledged, for reclaim to take up once it has
	// been idle visibilityTimeout.
	maxQueuedPerTarget = 64
	maxQueued          = 256
	// queueMaxWait bounds how long after its read a delivery may start:
	// one that has not started by then, waiting for its target's slot or
	// for a free worker, is left pending instead (handOut, next).
	queueMaxWait = time.Minute

	// visibilityTimeout is how long a delivery may stay unacknowledged
	// before another consumer takes it over. It outlasts the longest a
	// live pod holds an entry it has read: started within queueMaxWait of
	// the read, ended within deliveryBudget, acknowledged within
	// ackTimeout. Otherwise that work would be claimed and sent twice
	// (TestVisibilityTimeoutOutlastsWhatAPodHolds).
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
// platform's URL carries one. Webhook-Id and Webhook-Timestamp are
// covered too: UCP after 2026-08-25 requires it of an order webhook and
// has platforms reject a delivery whose signature omits either, and a
// verifier of 2026-08-25 rebuilds the base from the listed components,
// so covering two more costs it nothing.
func signedComponents(withQuery bool) []string {
	c := []string{"@method", "@authority", "@path"}
	if withQuery {
		c = append(c, "@query")
	}
	return append(c, "ucp-agent", "content-digest", "content-type",
		"webhook-id", "webhook-timestamp")
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
		hc:       platformClient(allowLocal, postTimeout),
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
		d.log.ErrorContext(ctx, "webhook consumer group unavailable",
			slog.String("error", err.Error()))
	}

	// Deliveries outlive the consume loop by up to drainTimeout: a
	// shutdown lets a post in flight finish instead of abandoning it for
	// a duplicate from whichever consumer takes it over.
	deliveryCtx, cancelDeliveries := context.WithCancel(
		context.WithoutCancel(ctx))
	jobs := make(chan job)
	slots := newTargetSlots(perTargetDeliveries, maxQueuedPerTarget,
		maxQueued)
	var wg sync.WaitGroup
	for range deliveryWorkers {
		wg.Go(func() {
			for j := range jobs {
				// Then the target's queue, in order, on the slot this
				// delivery held; none once the consume loop has stopped.
				for ok := true; ok; {
					budget, cancel := context.WithTimeout(deliveryCtx,
						deliveryBudget)
					d.deliver(budget, j.id, j.dl)
					cancel()
					var stale []job
					j, stale, ok = slots.next(j.target, time.Now(),
						ctx.Err() == nil)
					for _, s := range stale {
						d.logLeftPending(ctx, s, "it was not started in time")
					}
				}
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
	// A reclaim sweep runs every reclaimInterval and goes on from where
	// its last call stopped until it has scanned every pending entry, so
	// entries past one call's window are reached too.
	reclaimFrom := "0-0"
	var lastReclaim time.Time
	for ctx.Err() == nil {
		var msgs []redis.XMessage
		// When Redis handed the entries over, at the latest: stamped
		// before each call, so a delivery's wait counts from no later than
		// the moment its idle time was reset.
		readAt := time.Now()
		switch {
		case pendingFrom != "":
			msgs = d.read(ctx, pendingFrom)
			if len(msgs) == 0 {
				pendingFrom = ""
				continue
			}
			pendingFrom = msgs[len(msgs)-1].ID
		case reclaimFrom != "0-0" ||
			time.Since(lastReclaim) >= reclaimInterval:
			if reclaimFrom == "0-0" {
				lastReclaim = time.Now()
			}
			msgs, reclaimFrom = d.reclaim(ctx, reclaimFrom)
		}
		if len(msgs) == 0 && pendingFrom == "" {
			readAt = time.Now()
			msgs = d.read(ctx, ">")
		}
		batch := make([]job, 0, len(msgs))
		for _, msg := range msgs {
			if dl, ok := d.decode(ctx, msg); ok {
				batch = append(batch, job{id: msg.ID, dl: dl,
					target: deliveryTarget(dl.TargetURL), readAt: readAt})
			}
		}
		if !d.handOut(ctx, jobs, slots, batch, queueMaxWait) {
			return
		}
	}
}

// handOut gives each job of a batch to a worker, or queues it behind its
// target's slots, or leaves it pending for reclaim. Whatever it gives a
// worker starts within maxWait of its read: a job older than that, or
// one no worker takes by then (every worker busy), is left pending, its
// slot freed. It returns false once ctx has ended; what it had not
// handed out stays pending.
func (d *Dispatcher) handOut(
	ctx context.Context, jobs chan<- job, slots *targetSlots, batch []job,
	maxWait time.Duration,
) bool {
	for _, j := range batch {
		deadline := j.readAt.Add(maxWait)
		wait := time.Until(deadline)
		if wait <= 0 {
			d.logLeftPending(ctx, j, "it waited too long")
			continue
		}
		switch slots.admit(j) {
		case queued:
			continue
		case refused:
			d.logLeftPending(ctx, j, "its target's queue is full")
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case jobs <- j:
			timer.Stop()
		case <-timer.C:
			d.logLeftPending(ctx, j, "no worker was free in time")
			for _, q := range slots.release(j.target) {
				d.logLeftPending(ctx, q, "its target has nothing in flight")
			}
		case <-ctx.Done():
			timer.Stop()
			slots.release(j.target)
			return false
		}
	}
	return true
}

// logLeftPending records a delivery left pending for reclaim.
func (d *Dispatcher) logLeftPending(ctx context.Context, j job, why string) {
	d.log.InfoContext(ctx, "webhook delivery left pending",
		slog.String("reason", why),
		slog.String("delivery", j.dl.ID),
		slog.String("target", j.target),
		slog.String(obs.KeyTenant, j.dl.Schema))
}

// job is one delivery: its stream entry, the delivery, its target and
// when it was read.
type job struct {
	id     string
	dl     Delivery
	target string
	readAt time.Time
}

// admission is what targetSlots.admit did with a delivery.
type admission int

const (
	// admitted: it holds a slot of its target; hand it to a worker.
	admitted admission = iota
	// queued: it waits in its target's queue.
	queued
	// refused: the queue is full; leave it pending.
	refused
)

// targetSlots counts the deliveries each target has in flight, up to a
// cap, and queues the ones read beyond it, per target and in order.
// Targets with nothing in flight are forgotten, so the maps hold only
// what is in flight or queued.
type targetSlots struct {
	cap, perTarget, total int

	mu     sync.Mutex
	held   map[string]int
	queue  map[string][]job
	queued int
}

func newTargetSlots(capacity, perTarget, total int) *targetSlots {
	return &targetSlots{
		cap: capacity, perTarget: perTarget, total: total,
		held: make(map[string]int), queue: make(map[string][]job),
	}
}

// admit takes a slot of j's target for j, or queues j behind the target's
// slots, or refuses it when the queues are full.
func (s *targetSlots) admit(j job) admission {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[j.target] < s.cap {
		s.held[j.target]++
		return admitted
	}
	if len(s.queue[j.target]) >= s.perTarget || s.queued >= s.total {
		return refused
	}
	s.queue[j.target] = append(s.queue[j.target], j)
	s.queued++
	return queued
}

// next is called by a worker that finished a delivery to target. With
// more, it returns the target's next queued delivery, which keeps the
// slot; otherwise, or with none queued, it frees the slot. A queued
// delivery read more than queueMaxWait before now, or left queued when
// the target's last slot is freed, is dropped from the queue and
// returned as stale, to be left pending for reclaim.
func (s *targetSlots) next(
	target string, now time.Time, more bool,
) (job, []job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var stale []job
	q := s.queue[target]
	for more && len(q) > 0 {
		j := q[0]
		q = q[1:]
		s.queued--
		if now.Sub(j.readAt) > queueMaxWait {
			stale = append(stale, j)
			continue
		}
		s.setQueue(target, q)
		return j, stale, true
	}
	s.setQueue(target, q)
	return job{}, append(stale, s.releaseLocked(target)...), false
}

// release frees a slot of target, and returns what releaseLocked dropped.
func (s *targetSlots) release(target string) []job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releaseLocked(target)
}

// releaseLocked frees a slot of target. Freeing its last also drops its
// queue and returns it: with nothing of the target in flight, nothing
// would take the next one, and a free slot must mean an empty queue.
// The dropped deliveries are left pending for reclaim.
func (s *targetSlots) releaseLocked(target string) []job {
	if s.held[target] > 1 {
		s.held[target]--
		return nil
	}
	delete(s.held, target)
	dropped := s.queue[target]
	s.queued -= len(dropped)
	delete(s.queue, target)
	return dropped
}

func (s *targetSlots) setQueue(target string, q []job) {
	if len(q) == 0 {
		delete(s.queue, target)
		return
	}
	s.queue[target] = q
}

// deliveryTarget names whose endpoint a delivery waits on, for the cap
// on deliveries in flight to one target. It is the registrable domain of
// the URL's host (eTLD+1 by the public suffix list, whose private section
// makes each customer of a hosting domain its own), because a name under
// one's own domain costs nothing: a cap per host or per origin would give
// whoever controls a domain as many shares of the workers as names, or
// ports, it cares to use. An address is its holder as the rate limiter
// counts one (httpmw.AddrBucket): an IPv4 address, an IPv6 /64. A host
// the list cannot place (a single label, as in development) is itself.
// An internationalised name is read as its A-label, so its spellings are
// one target.
func deliveryTarget(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		return httpmw.AddrBucket(addr.WithZone(""))
	}
	// One name in any of its spellings: Unicode, its case, and its
	// A-label (IDNA2008 lookup mapping).
	if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		host = ascii
	}
	if site, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return site
	}
	return host
}

func (d *Dispatcher) ensureGroup(ctx context.Context) error {
	err := d.rdb.XGroupCreateMkStream(ctx, eventsStream, eventsGroup, "0").
		Err()
	// BUSYGROUP: another pod (or an earlier boot) created it.
	if err != nil && !redis.HasErrorPrefix(err, "BUSYGROUP") {
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
	d.log.WarnContext(ctx, "webhook queue read failed",
		slog.String("error", err.Error()))
	_ = d.ensureGroup(ctx)
	select {
	case <-time.After(time.Second):
	case <-ctx.Done():
	}
	return nil
}

// reclaim takes over deliveries a consumer (this one included) has held
// past visibilityTimeout, scanning the pending entries from from, and
// returns them with where the sweep goes on: "0-0" once it has scanned
// them all. A call scans a bounded window, so a sweep takes as many calls
// as the pending entries need. It also removes consumers long gone.
func (d *Dispatcher) reclaim(
	ctx context.Context, from string,
) ([]redis.XMessage, string) {
	msgs, next, err := d.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   eventsStream,
		Group:    eventsGroup,
		Consumer: d.consumer,
		MinIdle:  visibilityTimeout,
		Start:    from,
		Count:    deliveryWorkers,
	}).Result()
	if err != nil {
		if ctx.Err() == nil {
			d.log.WarnContext(ctx, "webhook reclaim failed",
				slog.String("error", err.Error()))
		}
		next = "0-0"
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
	return msgs, next
}

// decode reads a stream entry's delivery. One that cannot be delivered
// as stored is acknowledged and dropped.
func (d *Dispatcher) decode(
	ctx context.Context, msg redis.XMessage,
) (Delivery, bool) {
	raw, _ := msg.Values[deliveryField].(string)
	var dl Delivery
	if err := json.Unmarshal([]byte(raw), &dl); err != nil ||
		dl.Schema == "" || dl.TargetURL == "" {
		d.log.ErrorContext(ctx, "webhook delivery corrupt",
			slog.String("entry", msg.ID))
		d.ack(msg.ID)
		return Delivery{}, false
	}
	return dl, true
}

// deliver attempts one delivery of the entry id and acknowledges it ONLY
// on a terminal outcome: delivered, or refused by the endpoint on every
// attempt. A shutdown that aborts delivery first, or a signing key that
// cannot be read, leaves the entry pending for reclaim — never
// acknowledged, which would drop an undelivered order webhook and break
// at-least-once.
func (d *Dispatcher) deliver(ctx context.Context, id string, dl Delivery) {
	keyFailed := false
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
			d.log.WarnContext(ctx, "webhook signing key unavailable",
				slog.String(obs.KeyTenant, dl.Schema),
				slog.String("error", err.Error()))
			keyFailed = true
			continue
		}
		if d.post(ctx, key, dl) {
			d.ack(id)
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
	if keyFailed {
		// Not every attempt reached the endpoint: the store's key could
		// not be read (Redis, or a sealed record that does not open).
		// Never a terminal outcome; reclaim tries again.
		d.log.ErrorContext(ctx, "webhook delivery left pending: the "+
			"store's signing key is unavailable",
			slog.String("delivery", dl.ID),
			slog.String(obs.KeyTenant, dl.Schema))
		return
	}
	// Retries exhausted against a live endpoint: give up so the entry
	// does not cycle forever. There is no dead-letter stream by design;
	// this permanent-failure log is the record.
	d.log.ErrorContext(ctx, "webhook delivery failed permanently",
		slog.String("delivery", dl.ID),
		slog.String(obs.KeyTenant, dl.Schema))
	d.ack(id)
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
		d.log.WarnContext(ctx, "webhook ack failed; the delivery will repeat",
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
		d.log.ErrorContext(ctx, "webhook signing failed",
			slog.String("delivery", dl.ID), slog.String("error", err.Error()))
		return false
	}

	resp, err := d.hc.Do(req)
	if err != nil {
		d.log.WarnContext(ctx, "webhook post failed",
			slog.String("delivery", dl.ID), slog.String("error", err.Error()))
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the keep-alive connection is reusable for the next
	// delivery to the same platform.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
