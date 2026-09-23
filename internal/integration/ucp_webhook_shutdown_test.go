//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// shutdownPlatform is a platform webhook endpoint that holds deliveries
// open until the request is cancelled, then serves 200 once armed to
// succeed. It lets the test park a worker mid-delivery, stop the
// dispatcher, and confirm the delivery survives to the next boot.
type shutdownPlatform struct {
	firstHit  chan struct{} // signals a request has arrived
	succeed   atomic.Bool   // once true, requests get a 200
	delivered chan []byte   // bodies of successful deliveries
}

func newShutdownPlatform() *shutdownPlatform {
	return &shutdownPlatform{
		firstHit:  make(chan struct{}, 8),
		delivered: make(chan []byte, 8),
	}
}

func (p *shutdownPlatform) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case p.firstHit <- struct{}{}:
		default:
		}
		if p.succeed.Load() {
			p.delivered <- body
			w.WriteHeader(http.StatusOK)
			return
		}
		<-r.Context().Done()
	})
}

// TestDispatcherSurvivesShutdownMidDelivery proves at-least-once across a
// shutdown: a delivery in flight when the dispatcher stops is never
// acknowledged, stays pending on its consumer, and the same consumer
// resumes it on its next boot.
func TestDispatcherSurvivesShutdownMidDelivery(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()

	platform := newShutdownPlatform()
	server := httptest.NewServer(platform.handler())
	t.Cleanup(server.Close)

	keys := ucp.NewKeys(rdb)
	const consumer = "gateway-pod-0"
	body := json.RawMessage(`{"id":"b9be45e5-6062-4976-ae7b-2c31eb2ad689"}`)
	require.NoError(t, ucp.NewDispatcher(rdb, keys, quietLogger(),
		consumer, true).Enqueue(ctx, ucp.Delivery{
		ID:        "7d1f4c2e-0b8a-5c3e-9f61-2a4b6c8d0e12",
		Schema:    "demostore",
		Domain:    "shop.example.test",
		TargetURL: server.URL + "/ucp/orders",
		Body:      body,
	}))

	// Boot #1: the platform holds the request, parking a worker.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		ucp.NewDispatcher(rdb, keys, quietLogger(), consumer, true).
			Run(runCtx)
		close(done)
	}()
	select {
	case <-platform.firstHit:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatcher never attempted delivery")
	}

	// Shutdown mid-delivery: Run returns once the drain window ends.
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("dispatcher did not stop after context cancel")
	}
	pending, err := rdb.XPending(ctx, "ag:webhooks:orders", "dispatch").Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, pending.Count,
		"an undelivered webhook must stay pending, never acknowledged")

	// Boot #2 under the same consumer name: it resumes its own pending
	// delivery immediately.
	platform.succeed.Store(true)
	runCtx2, cancel2 := context.WithCancel(ctx)
	t.Cleanup(cancel2)
	go ucp.NewDispatcher(rdb, keys, quietLogger(), consumer, true).
		Run(runCtx2)

	select {
	case got := <-platform.delivered:
		assert.JSONEq(t, string(body), string(got))
	case <-time.After(15 * time.Second):
		t.Fatal("pending delivery was not resumed on the next boot")
	}
	assert.Eventually(t, func() bool {
		n, err := rdb.XLen(ctx, "ag:webhooks:orders").Result()
		return err == nil && n == 0
	}, 5*time.Second, 50*time.Millisecond,
		"a delivered webhook is acknowledged and deleted")
}
