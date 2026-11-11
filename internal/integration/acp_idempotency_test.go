//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionCount is how many checkout sessions the store holds.
func (stack ucpStack) sessionCount(t *testing.T) int {
	t.Helper()
	keys, err := stack.rdb.Keys(context.Background(),
		sessionRedisKey("*")).Result()
	require.NoError(t, err)
	n := 0
	for _, key := range keys {
		if !strings.HasSuffix(key, ":lock") {
			n++
		}
	}
	return n
}

// rawAnswer is one answer read in full, from any goroutine.
type rawAnswer struct {
	status   int
	replayed string
	body     []byte
	err      error
}

// acpSend sends req and reads its answer in full. It asserts nothing, so
// it runs on any goroutine.
func acpSend(req *http.Request) rawAnswer {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return rawAnswer{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	a := rawAnswer{
		status:   resp.StatusCode,
		replayed: resp.Header.Get("Idempotent-Replayed"),
	}
	a.body, a.err = io.ReadAll(resp.Body)
	return a
}

// concurrently sends n copies of the same create at once.
func concurrently(
	t *testing.T, n int, url string, body any, key string,
) []rawAnswer {
	t.Helper()
	answers := make([]rawAnswer, n)
	var wg sync.WaitGroup
	for i := range answers {
		req := acpRequest(t, http.MethodPost, url, body, key)
		wg.Go(func() { answers[i] = acpSend(req) })
	}
	wg.Wait()
	for _, a := range answers {
		require.NoError(t, a.err)
	}
	return answers
}

// The spec keeps a key's response for at least 24 hours: "Same key +
// identical body → server MUST return the original response with the
// same HTTP status code", and "MUST NOT re-execute side effects". A key
// whose request failed, or was refused, is free for the next attempt.
func TestACPIdempotencyKeys(t *testing.T) {
	stack := startUCPGateway(t)
	base := stack.gw.URL + "/acp/checkout_sessions"
	ctx := context.Background()
	createBody := map[string]any{
		"line_items":   []map[string]any{{"id": "1", "quantity": 2}},
		"currency":     "EUR",
		"capabilities": map[string]any{},
	}
	// create and assertFresh take the subtest's t: a helper that
	// failed the parent from a subtest would misname the failure.
	create := func(
		t *testing.T, key string, body any,
	) (*http.Response, []byte) {
		t.Helper()
		return acpRaw(t, acpRequest(t, http.MethodPost, base, body, key))
	}
	assertFresh := func(t *testing.T, resp *http.Response, body []byte) {
		t.Helper()
		assert.Equal(t, http.StatusCreated, resp.StatusCode, "%s", body)
		assert.Empty(t, resp.Header.Get("Idempotent-Replayed"))
	}

	t.Run("a create replay answers the original response", func(t *testing.T) {
		key := uuid.NewString()
		resp, original := create(t, key, createBody)
		assertFresh(t, resp, original)
		var created map[string]any
		require.NoError(t, json.Unmarshal(original, &created))
		mints := stack.faults.cartCreates.Load()

		assertReplay := func(resp *http.Response, body []byte) {
			t.Helper()
			assert.Equal(t, http.StatusCreated, resp.StatusCode)
			assert.Equal(t, "true", resp.Header.Get("Idempotent-Replayed"))
			assert.Equal(t, string(original), string(body))
		}
		resp, body := create(t, key, createBody)
		assertReplay(resp, body)

		// Its session gone, the key still answers what it answered.
		stack.lapse(t, sessionRedisKey(created["id"].(string)))
		sessions := stack.sessionCount(t)
		resp, body = create(t, key, createBody)
		assertReplay(resp, body)
		for _, a := range concurrently(t, 2, base, createBody, key) {
			assert.Equal(t, http.StatusCreated, a.status)
			assert.Equal(t, "true", a.replayed)
			assert.Equal(t, string(original), string(a.body))
		}
		assert.Equal(t, mints, stack.faults.cartCreates.Load(),
			"a replay must not create a cart")
		assert.Equal(t, sessions, stack.sessionCount(t),
			"a replay must not store a session")

		other := map[string]any{
			"line_items":   []map[string]any{{"id": "1", "quantity": 3}},
			"currency":     "EUR",
			"capabilities": map[string]any{},
		}
		resp, refused := acpCall(t, http.MethodPost, base, other, key)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
		assert.Equal(t, "idempotency_conflict", refused["code"])
		assert.Empty(t, resp.Header.Get("Idempotent-Replayed"))
	})

	t.Run("concurrent creates with a new key make one session",
		func(t *testing.T) {
			mints, sessions := stack.faults.cartCreates.Load(),
				stack.sessionCount(t)
			answers := concurrently(t, 6, base, createBody, uuid.NewString())
			var fresh []byte
			for _, a := range answers {
				if a.status == http.StatusCreated && a.replayed == "" {
					require.Nil(t, fresh, "two answers created a session")
					fresh = a.body
				}
			}
			require.NotNil(t, fresh, "no answer created the session")
			for _, a := range answers {
				switch {
				case a.status == http.StatusConflict:
					var e map[string]any
					require.NoError(t, json.Unmarshal(a.body, &e))
					assert.Equal(t, "idempotency_in_flight", e["code"])
				case a.replayed == "true":
					assert.Equal(t, http.StatusCreated, a.status)
					assert.Equal(t, string(fresh), string(a.body))
				default:
					assert.Equal(t, string(fresh), string(a.body),
						"status %d", a.status)
				}
			}
			assert.EqualValues(t, 1, stack.faults.cartCreates.Load()-mints)
			assert.Equal(t, sessions+1, stack.sessionCount(t))
		})

	t.Run("a refused create leaves its key free", func(t *testing.T) {
		key := uuid.NewString()
		empty := map[string]any{
			"line_items": []map[string]any{}, "currency": "EUR",
			"capabilities": map[string]any{},
		}
		for range 2 {
			resp, body := acpCall(t, http.MethodPost, base, empty, key)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Equal(t, "missing", body["code"])
			assert.Empty(t, resp.Header.Get("Idempotent-Replayed"))
		}
		resp, body := create(t, key, createBody)
		assertFresh(t, resp, body)
	})

	t.Run("a create that cannot be answered stores nothing",
		func(t *testing.T) {
			key := uuid.NewString()
			sessions := stack.sessionCount(t)
			// The create's own read of the cart passes; the render's fails.
			stack.faults.namedCartReadsFailFrom.Store(
				stack.faults.namedCartReads.Load() + 2)
			t.Cleanup(func() { stack.faults.namedCartReadsFailFrom.Store(0) })
			resp, body := acpCall(t, http.MethodPost, base, createBody, key)
			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			assert.Equal(t, "unavailable", body["code"])
			assert.Equal(t, sessions, stack.sessionCount(t),
				"a create that cannot be answered must not keep a session")

			stack.faults.namedCartReadsFailFrom.Store(0)
			resp, raw := create(t, key, createBody)
			assertFresh(t, resp, raw)
		})

	t.Run("an older pod's create record is not read", func(t *testing.T) {
		key := uuid.NewString()
		require.NoError(t, stack.rdb.Set(ctx,
			"ag:demostore:acp:idem:create:"+key,
			`{"sessionId":"`+uuid.NewString()+`","bodyHash":"x",`+
				`"status":201}`, time.Hour).Err())
		resp, body := create(t, key, createBody)
		assertFresh(t, resp, body)
	})

	// A claim lasts what its request can take when the store answers
	// every call just inside the upstream timeout: the lock's six, and
	// one more for the write that settles the key.
	t.Run("a settled key is kept 24 hours, a claim seven upstream timeouts",
		func(t *testing.T) {
			key := uuid.NewString()
			resp, body := create(t, key, createBody)
			assertFresh(t, resp, body)
			ttl, err := stack.rdb.TTL(ctx,
				"ag:demostore:acp:idem:create.v2:"+key).Result()
			require.NoError(t, err)
			assert.InDelta(t, (24 * time.Hour).Seconds(), ttl.Seconds(), 60)

			id := acpReady(t, base)
			reached, answer := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(answer) })
			defer release()
			hold := func(*http.Request) {
				close(reached)
				<-answer
			}
			stack.faults.orderHold.Store(&hold)
			t.Cleanup(func() { stack.faults.orderHold.Store(nil) })
			completeKey := uuid.NewString()
			done := make(chan int, 1)
			req := acpRequest(t, http.MethodPost, base+"/"+id+"/complete",
				map[string]any{"payment_data": map[string]any{}}, completeKey)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					done <- 0
					return
				}
				_ = resp.Body.Close()
				done <- resp.StatusCode
			}()
			select {
			case <-reached:
			case <-time.After(ucpUpstreamTimeout):
				t.Fatal("the order was never sent to the store")
			}
			ttl, err = stack.rdb.TTL(ctx,
				"ag:demostore:acp:idem:complete:"+id+":"+completeKey).Result()
			require.NoError(t, err)
			assert.Positive(t, ttl, "a claim must expire")
			assert.InDelta(t, (7 * ucpUpstreamTimeout).Seconds(),
				ttl.Seconds(), 2)
			lock, err := stack.rdb.TTL(ctx, lockRedisKey(id)).Result()
			require.NoError(t, err)
			assert.InDelta(t, (6 * ucpUpstreamTimeout).Seconds(),
				lock.Seconds(), 2)
			release()
			assert.Equal(t, http.StatusOK, <-done)
		})

	t.Run("a cancel refused with 405 leaves its key free", func(t *testing.T) {
		resp, created := acpCall(t, http.MethodPost, base, createBody,
			uuid.NewString())
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		id := created["id"].(string)
		resp, _ = acpCall(t, http.MethodPost, base+"/"+id+"/cancel", nil,
			uuid.NewString())
		require.Equal(t, http.StatusOK, resp.StatusCode)

		key := uuid.NewString()
		for range 2 {
			resp, _ = acpCall(t, http.MethodPost, base+"/"+id+"/cancel", nil,
				key)
			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
			assert.Empty(t, resp.Header.Get("Idempotent-Replayed"))
		}
		n, err := stack.rdb.Exists(ctx,
			"ag:demostore:acp:idem:cancel:"+id+":"+key).Result()
		require.NoError(t, err)
		assert.Zero(t, n)
	})
}
