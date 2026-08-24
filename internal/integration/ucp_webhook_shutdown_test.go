//go:build integration

package integration

import (
	"context"
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

// shutdownPlatform is a platform webhook endpoint that blocks the first
// delivery until the request is cancelled, then serves 200 once armed to
// succeed. It lets the test park a worker mid-delivery, cancel the
// dispatcher, and confirm the event is later redelivered on "next boot".
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
		// Not yet armed: hold the request open so the worker is parked
		// mid-delivery until the dispatcher's context is cancelled (which
		// cancels the client request and, with it, this server context).
		<-r.Context().Done()
	})
}

// TestDispatcherSurvivesShutdownMidDelivery proves the at-least-once
// guarantee across a shutdown: an order webhook that is in flight when the
// dispatcher's context is cancelled must NOT be lost — it stays recoverable
// on the processing (or events) list and is redelivered on the next boot.
func TestDispatcherSurvivesShutdownMidDelivery(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()

	// Clean slate: these keys are process-wide, not tenant-scoped.
	require.NoError(t, rdb.Del(ctx,
		"ag:events:orders", "ag:events:orders:processing").Err())

	platform := newShutdownPlatform()
	server := httptest.NewServer(platform.handler())
	t.Cleanup(server.Close)

	keys := ucp.NewKeys(rdb)
	dispatcher := ucp.NewDispatcher(rdb, keys, quietLogger())

	const orderUUID = "b9be45e5-6062-4976-ae7b-2c31eb2ad689"
	require.NoError(t, dispatcher.Enqueue(ctx, ucp.OrderEvent{
		Schema:    "webside",
		OrderUUID: orderUUID,
		Status:    "PROCESSING",
		TargetURL: server.URL + "/ucp/orders",
	}))

	// Boot #1: the platform blocks, so the worker parks mid-delivery.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { dispatcher.Run(runCtx); close(done) }()

	select {
	case <-platform.firstHit:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatcher never attempted delivery")
	}

	// Shutdown mid-delivery, then wait for the worker pool to drain.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatcher did not stop after context cancel")
	}

	// The undelivered event must still be recoverable, not dropped.
	pending := rdb.LLen(ctx, "ag:events:orders").Val() +
		rdb.LLen(ctx, "ag:events:orders:processing").Val()
	assert.Equal(t, int64(1), pending,
		"in-flight event must survive shutdown on the events or processing list")

	// Boot #2: arm the platform to succeed. reclaim() requeues the stranded
	// event and the fresh dispatcher redelivers it — at-least-once holds.
	platform.succeed.Store(true)
	runCtx2, cancel2 := context.WithCancel(ctx)
	t.Cleanup(cancel2)
	go ucp.NewDispatcher(rdb, keys, quietLogger()).Run(runCtx2)

	select {
	case body := <-platform.delivered:
		assert.Contains(t, string(body), orderUUID)
	case <-time.After(15 * time.Second):
		t.Fatal("event was not redelivered on the next boot")
	}

	// Once delivered, both lists drain (the event is acknowledged).
	assert.Eventually(t, func() bool {
		return rdb.LLen(ctx, "ag:events:orders").Val() == 0 &&
			rdb.LLen(ctx, "ag:events:orders:processing").Val() == 0
	}, 5*time.Second, 50*time.Millisecond,
		"delivered event must be removed from both lists")
}
