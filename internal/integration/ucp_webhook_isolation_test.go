//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

const webhookStream = "ag:webhooks:orders"

// stallingPlatform is a webhook endpoint that holds every request open
// until it is released, then answers 200, and counts how many it holds
// at once.
type stallingPlatform struct {
	release chan struct{}
	once    sync.Once
	hits    atomic.Int32
	held    atomic.Int32
	mu      sync.Mutex
	maxHeld int32
}

func (p *stallingPlatform) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		p.hits.Add(1)
		n := p.held.Add(1)
		p.mu.Lock()
		p.maxHeld = max(p.maxHeld, n)
		p.mu.Unlock()
		defer p.held.Add(-1)
		select {
		case <-p.release:
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	})
}

func (p *stallingPlatform) mostHeld() int32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxHeld
}

func (p *stallingPlatform) answer() { p.once.Do(func() { close(p.release) }) }

// serveOn starts h on addr, an address of the loopback network: one
// target per address, as the dispatcher counts them.
func serveOn(t *testing.T, addr string, h http.Handler) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// An endpoint that does not answer holds at most its share of the
// workers: more of its deliveries than there are workers wait in the
// stream, unacknowledged and read once, while another platform's
// delivery goes out at once. Once the endpoint answers, the rest go out
// from the target's queue, each once.
func TestDispatcherIsolatesAStalledTarget(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	keys := newKeys(t, rdb, testKEK(t))

	stalled := &stallingPlatform{release: make(chan struct{})}
	// 127.0.0.2: another target than the healthy platform's 127.0.0.1.
	stalledSrv := serveOn(t, "127.0.0.2:0", stalled.handler())
	t.Cleanup(stalled.answer)
	delivered := make(chan []byte, 1)
	healthySrv := serveOn(t, "127.0.0.1:0", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			delivered <- body
			w.WriteHeader(http.StatusOK)
		}))

	const consumer = "gateway-pod-0"
	enqueue := ucp.NewDispatcher(rdb, keys, quietLogger(), consumer, true)
	// More stalled deliveries than the pool has workers, queued first.
	const stalledEvents = 12
	for i := range stalledEvents {
		require.NoError(t, enqueue.Enqueue(ctx, ucp.Delivery{
			ID:        fmt.Sprintf("stalled-%02d", i),
			Schema:    "alpha",
			Domain:    "alpha.example.test",
			TargetURL: stalledSrv.URL + "/ucp/orders",
			Body:      json.RawMessage(`{"n":` + strconv.Itoa(i) + `}`),
		}))
	}
	healthyBody := json.RawMessage(`{"id":"healthy"}`)
	require.NoError(t, enqueue.Enqueue(ctx, ucp.Delivery{
		ID:        "healthy",
		Schema:    "beta",
		Domain:    "beta.example.test",
		TargetURL: healthySrv.URL + "/ucp/orders",
		Body:      healthyBody,
	}))

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		ucp.NewDispatcher(rdb, keys, quietLogger(), consumer, true).
			Run(runCtx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// The stalled target holds its deliveries for the client's whole
	// timeout; the other platform's goes out long before that.
	select {
	case got := <-delivered:
		assert.JSONEq(t, string(healthyBody), string(got))
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled target held up another platform's delivery")
	}

	// It holds no more than its share, and nothing else of it is tried.
	time.Sleep(time.Second)
	assert.EqualValues(t, 2, stalled.mostHeld(),
		"a target holds at most its share of the workers")
	assert.EqualValues(t, 2, stalled.hits.Load(),
		"a deferred delivery is not posted")

	// Every stalled delivery is still in the stream, unacknowledged, and
	// was read once: a deferred entry is neither acked nor read again
	// and again.
	n, err := rdb.XLen(ctx, webhookStream).Result()
	require.NoError(t, err)
	assert.EqualValues(t, stalledEvents, n)
	pending, err := rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: webhookStream, Group: "dispatch",
		Start: "-", End: "+", Count: 100,
	}).Result()
	require.NoError(t, err)
	require.Len(t, pending, stalledEvents)
	for _, p := range pending {
		assert.Equal(t, consumer, p.Consumer)
		assert.EqualValues(t, 1, p.RetryCount,
			"a deferred delivery is read once, not polled")
	}

	// The endpoint answers again: the two in flight are delivered, and
	// the workers take the rest from the target's queue, each once, two
	// at a time, without a restart or the visibility timeout.
	stalled.answer()
	assert.Eventually(t, func() bool {
		n, err := rdb.XLen(ctx, webhookStream).Result()
		return err == nil && n == 0
	}, 10*time.Second, 50*time.Millisecond,
		"every queued delivery goes out once its target answers")
	assert.EqualValues(t, stalledEvents, stalled.hits.Load(),
		"each delivery is posted once")
	assert.LessOrEqual(t, stalled.mostHeld(), int32(2))
}

// countingPlatform answers every delivery after a delay, and counts the
// deliveries by Webhook-Id.
type countingPlatform struct {
	delay time.Duration
	mu    sync.Mutex
	seen  map[string]int
	held  atomic.Int32
	most  atomic.Int32
}

func (p *countingPlatform) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := p.held.Add(1)
		defer p.held.Add(-1)
		for {
			m := p.most.Load()
			if n <= m || p.most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(p.delay)
		p.mu.Lock()
		p.seen[r.Header.Get("Webhook-Id")]++
		p.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
}

func (p *countingPlatform) counts() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.seen))
	for k, v := range p.seen {
		out[k] = v
	}
	return out
}

// runDispatcher runs a dispatcher as consumer until the test ends.
func runDispatcher(t *testing.T, rdb *redis.Client, keys *ucp.Keys,
	consumer string,
) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ucp.NewDispatcher(rdb, keys, quietLogger(), consumer, true).Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// A burst to one healthy platform goes out as fast as the platform
// answers: two at a time, the rest from the target's queue, with no wait
// for the visibility timeout, every event once.
func TestDispatcherDeliversABurstToOneTargetPromptly(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	keys := newKeys(t, rdb, testKEK(t))
	platform := &countingPlatform{delay: 200 * time.Millisecond,
		seen: map[string]int{}}
	srv := serveOn(t, "127.0.0.1:0", platform.handler())

	const events = 20
	enqueue := ucp.NewDispatcher(rdb, keys, quietLogger(), "pod-0", true)
	want := map[string]int{}
	for i := range events {
		id := fmt.Sprintf("burst-%02d", i)
		want[id] = 1
		require.NoError(t, enqueue.Enqueue(ctx, ucp.Delivery{
			ID: id, Schema: "alpha", Domain: "alpha.example.test",
			TargetURL: srv.URL + "/ucp/orders",
			Body:      json.RawMessage(`{"n":` + strconv.Itoa(i) + `}`),
		}))
	}
	runDispatcher(t, rdb, keys, "pod-0")

	// 20 events, two at a time, 200ms each: about 2s.
	assert.Eventually(t, func() bool {
		n, err := rdb.XLen(ctx, webhookStream).Result()
		return err == nil && n == 0
	}, 15*time.Second, 50*time.Millisecond,
		"the burst is delivered without waiting out the visibility timeout")
	assert.Equal(t, want, platform.counts(), "every event once")
	assert.LessOrEqual(t, platform.most.Load(), int32(2))
}

// backdate makes the pending entry id, held by consumer, look idle for
// an hour, as a dead pod's entry does.
func backdate(t *testing.T, rdb *redis.Client, consumer, id string) {
	t.Helper()
	require.NoError(t, rdb.Do(context.Background(), "XCLAIM", webhookStream,
		"dispatch", consumer, 0, id, "IDLE", time.Hour.Milliseconds()).Err())
}

// readAs reads n new entries as consumer, which then holds them pending.
func readAs(t *testing.T, rdb *redis.Client, consumer string, n int64,
) []redis.XMessage {
	t.Helper()
	streams, err := rdb.XReadGroup(context.Background(),
		&redis.XReadGroupArgs{
			Group: "dispatch", Consumer: consumer,
			Streams: []string{webhookStream, ">"}, Count: n,
		}).Result()
	require.NoError(t, err)
	require.Len(t, streams, 1)
	require.Len(t, streams[0].Messages, int(n))
	return streams[0].Messages
}

// Reclaim reaches a dead pod's deliveries behind more pending entries
// than one scan window covers: a sweep goes on from where its last call
// stopped instead of starting over at the head.
func TestDispatcherReclaimReachesPastTheFirstScanWindow(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	keys := newKeys(t, rdb, testKEK(t))
	platform := &countingPlatform{seen: map[string]int{}}
	srv := serveOn(t, "127.0.0.1:0", platform.handler())
	enqueue := ucp.NewDispatcher(rdb, keys, quietLogger(), "pod-0", true)
	require.NoError(t, rdb.XGroupCreateMkStream(ctx, webhookStream,
		"dispatch", "0").Err())
	add := func(id string) {
		require.NoError(t, enqueue.Enqueue(ctx, ucp.Delivery{
			ID: id, Schema: "alpha", Domain: "alpha.example.test",
			TargetURL: srv.URL + "/ucp/orders", Body: json.RawMessage(`{}`),
		}))
	}

	// The head of the pending list: entries a live pod holds and is
	// working on, far more than one call scans.
	const busy = 200
	for i := range busy {
		add(fmt.Sprintf("busy-%03d", i))
	}
	readAs(t, rdb, "live-pod", busy)
	// Behind them, a dead pod's deliveries, idle long past the timeout.
	const dead = 3
	for i := range dead {
		add(fmt.Sprintf("dead-%d", i))
	}
	for _, msg := range readAs(t, rdb, "dead-pod", dead) {
		backdate(t, rdb, "dead-pod", msg.ID)
	}

	runDispatcher(t, rdb, keys, "pod-0")
	assert.Eventually(t, func() bool {
		c := platform.counts()
		return c["dead-0"] == 1 && c["dead-1"] == 1 && c["dead-2"] == 1
	}, 30*time.Second, 100*time.Millisecond,
		"a dead pod's deliveries are reclaimed behind the first window")
	for id := range platform.counts() {
		assert.NotContains(t, id, "busy-",
			"a live pod's deliveries are not taken over")
	}
}

// A delivery whose store's signing key cannot be read is never
// acknowledged: it is not a terminal outcome, and it stays pending for
// reclaim instead of being dropped.
func TestDispatcherKeepsADeliveryWhoseKeyIsUnavailable(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	keys := newKeys(t, rdb, testKEK(t))
	// A sealed record no key-encryption key of this test opens.
	require.NoError(t, rdb.Set(ctx, sealedKeyName("alpha"),
		base64.StdEncoding.EncodeToString(make([]byte, 60)), 0).Err())
	platform := &countingPlatform{seen: map[string]int{}}
	srv := serveOn(t, "127.0.0.1:0", platform.handler())

	enqueue := ucp.NewDispatcher(rdb, keys, quietLogger(), "pod-0", true)
	require.NoError(t, enqueue.Enqueue(ctx, ucp.Delivery{
		ID: "keyless", Schema: "alpha", Domain: "alpha.example.test",
		TargetURL: srv.URL + "/ucp/orders", Body: json.RawMessage(`{}`),
	}))
	runDispatcher(t, rdb, keys, "pod-0")

	// Three attempts, 5s and 20s apart, then it gives up for now.
	time.Sleep(28 * time.Second)
	assert.Empty(t, platform.counts(), "nothing is sent unsigned")
	n, err := rdb.XLen(ctx, webhookStream).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "the delivery is not dropped")
	pending, err := rdb.XPending(ctx, webhookStream, "dispatch").Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, pending.Count, "it stays pending for reclaim")
}
