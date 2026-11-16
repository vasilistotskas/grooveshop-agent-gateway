//go:build integration

package integration

import (
	"context"
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
// delivery goes out at once. They are not lost: the consumer's next boot
// replays them, its share at once, and leaves the rest pending for
// reclaim to take up after the visibility timeout.
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

	// The endpoint answers again: the two in flight are delivered and
	// acknowledged.
	stalled.answer()
	assert.Eventually(t, func() bool {
		n, err := rdb.XLen(ctx, webhookStream).Result()
		return err == nil && n == stalledEvents-2
	}, 5*time.Second, 50*time.Millisecond)
	cancel()
	<-done

	// The consumer's next boot replays what it holds: the target's share
	// at once, the rest still pending, never acknowledged undelivered.
	runCtx2, cancel2 := context.WithCancel(ctx)
	done2 := make(chan struct{})
	go func() {
		ucp.NewDispatcher(rdb, keys, quietLogger(), consumer, true).
			Run(runCtx2)
		close(done2)
	}()
	t.Cleanup(func() {
		cancel2()
		<-done2
	})
	assert.Eventually(t, func() bool {
		return stalled.hits.Load() == 4
	}, 5*time.Second, 50*time.Millisecond,
		"the replay delivers the target's share at once")
	time.Sleep(time.Second)
	assert.EqualValues(t, 4, stalled.hits.Load(),
		"the replay holds the target to its share")
	n, err = rdb.XLen(ctx, webhookStream).Result()
	require.NoError(t, err)
	assert.EqualValues(t, stalledEvents-4, n)
	left, err := rdb.XPending(ctx, webhookStream, "dispatch").Result()
	require.NoError(t, err)
	assert.EqualValues(t, stalledEvents-4, left.Count,
		"what the replay deferred stays pending for reclaim")
	assert.LessOrEqual(t, stalled.mostHeld(), int32(2))
}
