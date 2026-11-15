//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// sessionCount is how many checkout sessions the store holds.
func (stack ucpStack) sessionCount(t *testing.T) int {
	t.Helper()
	keys, err := stack.rdb.Keys(context.Background(),
		sessionRedisKey("*")).Result()
	require.NoError(t, err)
	n := 0
	for _, key := range keys {
		// Beside a session: its lock, and the order it placed.
		if !strings.HasSuffix(key, ":lock") &&
			!strings.HasSuffix(key, ":order") {
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

// redisAround wraps each Redis command: it decides whether the command
// is sent, and what its caller is told.
type redisAround func(cmd redis.Cmder, send func() error) error

func (redisAround) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (around redisAround) ProcessHook(
	next redis.ProcessHook,
) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		return around(cmd, func() error { return next(ctx, cmd) })
	}
}

func (redisAround) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return next
}

// A create answers 201 only once its key holds the response. When the
// write of that record fails, whether the key holds it is not known. The
// session is then not handed out, because a retry of the key could
// create a second one, and not discarded, because the record may name
// it. The retry replays it if it was recorded, and creates anew if not.
func TestACreateWhoseRecordIsNotConfirmedHandsOutNoSession(t *testing.T) {
	stack := startUCPGateway(t)
	base := stack.gw.URL + "/acp/checkout_sessions"
	body := map[string]any{
		"line_items":   []map[string]any{{"id": "1", "quantity": 2}},
		"currency":     "EUR",
		"capabilities": map[string]any{},
	}
	// Redis knows both scripts from here on: each is one command.
	resp, _ := acpCall(t, http.MethodPost, base, body, uuid.NewString())
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	for name, written := range map[string]bool{
		"the record was not written":             false,
		"the record was written, its reply lost": true,
	} {
		t.Run(name, func(t *testing.T) {
			key := uuid.NewString()
			record := "ag:demostore:acp:idem:create.v3:" + key
			var scripts atomic.Int32
			stack.rdb.AddHook(redisAround(
				func(cmd redis.Cmder, send func() error) error {
					// The key's first script claims it, its second
					// records the response.
					if !savesKey(cmd, record) || scripts.Add(1) != 2 {
						return send()
					}
					if written {
						if err := send(); err != nil {
							return err
						}
					}
					return errRedisDown
				}))
			sessions := stack.sessionCount(t)

			resp, refused := acpCall(t, http.MethodPost, base, body, key)

			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			assert.Equal(t, "unavailable", refused["code"])
			assert.Equal(t, sessions+1, stack.sessionCount(t),
				"the session is kept: the record may name it")

			if !written {
				// The key is still claimed, until the claim ends.
				resp, _ := acpCall(t, http.MethodPost, base, body, key)
				require.Equal(t, http.StatusConflict, resp.StatusCode)
				stack.lapse(t, record)
			}
			resp, created := acpCall(t, http.MethodPost, base, body, key)
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			stored := stack.storedSession(t, created["id"].(string))
			assert.Equal(t, checkout.StatusIncomplete, stored.Status)
			if written {
				assert.Equal(t, "true",
					resp.Header.Get("Idempotent-Replayed"))
				assert.Equal(t, sessions+1, stack.sessionCount(t))
			} else {
				assert.Empty(t, resp.Header.Get("Idempotent-Replayed"))
				assert.Equal(t, sessions+2, stack.sessionCount(t))
			}
		})
	}
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

	// The records the previous release wrote, under its scopes: a key
	// one of them settled runs afresh here, as after expiry.
	t.Run("an older pod's records are not read", func(t *testing.T) {
		key := uuid.NewString()
		require.NoError(t, stack.rdb.Set(ctx,
			"ag:demostore:acp:idem:create.v2:"+key,
			`{"bodyHash":"x","status":201,"body":"{}"}`, time.Hour).Err())
		resp, body := create(t, key, createBody)
		assertFresh(t, resp, body)

		id := acpReady(t, base)
		require.NoError(t, stack.rdb.Set(ctx,
			"ag:demostore:acp:idem:update:"+id+":"+key,
			`{"bodyHash":"x"}`, time.Hour).Err())
		resp, body = acpRaw(t, acpRequest(t, http.MethodPost, base+"/"+id,
			map[string]any{"buyer": map[string]any{
				"email": "other@example.test",
			}}, key))
		assert.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)
		assert.Empty(t, resp.Header.Get("Idempotent-Replayed"))
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
				"ag:demostore:acp:idem:create.v3:"+key).Result()
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
				"ag:demostore:acp:idem:complete.v3:"+id+":"+completeKey).Result()
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
			"ag:demostore:acp:idem:cancel.v3:"+id+":"+key).Result()
		require.NoError(t, err)
		assert.Zero(t, n)
	})
}

// Every POST's key keeps the response it sent: "Same key + identical
// body → server MUST return the original response with the same HTTP
// status code" (§6.3). A replay with an equivalent body gets those bytes,
// whatever the session became since. Equivalence is the body's JSON
// (§6.2): key order and "1.0" against "1" make no difference, null
// against an absent member does ("null means clear this field"), and so
// does any other change.
func TestEveryACPPostReplaysTheResponseItSent(t *testing.T) {
	stack := startUCPGateway(t)
	base := stack.gw.URL + "/acp/checkout_sessions"
	created := func(t *testing.T) string {
		t.Helper()
		resp, body := acpCall(t, http.MethodPost, base, map[string]any{
			"line_items":   []map[string]any{{"id": "1", "quantity": 2}},
			"currency":     "EUR",
			"capabilities": map[string]any{},
		}, uuid.NewString())
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		return body["id"].(string)
	}
	// Ways to change a session after its first answer.
	updated := func(t *testing.T, id string) {
		t.Helper()
		resp, body := acpCall(t, http.MethodPost, base+"/"+id,
			map[string]any{"buyer": map[string]any{
				"email": "later@example.test",
			}}, uuid.NewString())
		require.Equal(t, http.StatusOK, resp.StatusCode, "%v", body)
	}
	gone := func(t *testing.T, id string) {
		t.Helper()
		stack.lapse(t, sessionRedisKey(id))
	}

	cases := []struct {
		name string
		// target prepares a session and names the URL, and the session
		// when it exists before the call.
		target                                 func(t *testing.T) (url, id string)
		status                                 int
		body, equivalent, nullForAbsent, other string
		change                                 func(t *testing.T, id string, first []byte)
	}{
		{
			name:   "create",
			target: func(*testing.T) (string, string) { return base, "" },
			status: http.StatusCreated,
			body: `{"line_items":[{"id":"1","quantity":2}],` +
				`"currency":"EUR","capabilities":{},"ext":{"n":1}}`,
			equivalent: `{"ext":{"n":1.0},"capabilities":{},` +
				`"currency":"EUR","line_items":[{"quantity":2,"id":"1"}]}`,
			nullForAbsent: `{"line_items":[{"id":"1","quantity":2}],` +
				`"currency":"EUR","capabilities":{},"ext":{"n":1},` +
				`"buyer":null}`,
			other: `{"line_items":[{"id":"1","quantity":3}],` +
				`"currency":"EUR","capabilities":{},"ext":{"n":1}}`,
			change: func(t *testing.T, _ string, first []byte) {
				var session map[string]any
				require.NoError(t, json.Unmarshal(first, &session))
				updated(t, session["id"].(string))
			},
		},
		{
			name: "update",
			target: func(t *testing.T) (string, string) {
				id := acpReady(t, base)
				return base + "/" + id, id
			},
			status: http.StatusOK,
			body: `{"line_items":[{"id":"1","quantity":3}],` +
				`"buyer":{"email":"first@example.test"},"ext":{"n":1}}`,
			equivalent: `{"ext":{"n":1.0},` +
				`"buyer":{"email":"first@example.test"},` +
				`"line_items":[{"quantity":3,"id":"1"}]}`,
			nullForAbsent: `{"line_items":[{"id":"1","quantity":3}],` +
				`"buyer":{"email":"first@example.test"},"ext":{"n":1},` +
				`"discounts":null}`,
			other: `{"line_items":[{"id":"1","quantity":4}],` +
				`"buyer":{"email":"first@example.test"},"ext":{"n":1}}`,
			change: func(t *testing.T, id string, _ []byte) { updated(t, id) },
		},
		{
			name: "complete",
			target: func(t *testing.T) (string, string) {
				id := acpReady(t, base)
				return base + "/" + id + "/complete", id
			},
			status: http.StatusOK,
			body: `{"payment_data":{},` +
				`"buyer":{"email":"maria@example.test"},"ext":{"n":1}}`,
			equivalent: `{"ext":{"n":1.0},` +
				`"buyer":{"email":"maria@example.test"},"payment_data":{}}`,
			nullForAbsent: `{"payment_data":{},"buyer":{` +
				`"email":"maria@example.test","phone_number":null},` +
				`"ext":{"n":1}}`,
			other: `{"payment_data":{},` +
				`"buyer":{"email":"maria@example.test"},"ext":{"n":2}}`,
			change: func(t *testing.T, id string, _ []byte) { gone(t, id) },
		},
		{
			name: "cancel",
			target: func(t *testing.T) (string, string) {
				id := created(t)
				return base + "/" + id + "/cancel", id
			},
			status: http.StatusOK,
			body: `{"intent_trace":` +
				`{"reason_code":"buyer_cancelled","rank":1}}`,
			equivalent: `{"intent_trace":` +
				`{"rank":1.0,"reason_code":"buyer_cancelled"}}`,
			nullForAbsent: `{"intent_trace":{"reason_code":` +
				`"buyer_cancelled","rank":1,"description":null}}`,
			other:  `{"intent_trace":{"reason_code":"other","rank":1}}`,
			change: func(t *testing.T, id string, _ []byte) { gone(t, id) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, id := tc.target(t)
			key := uuid.NewString()
			send := func(body string) (*http.Response, []byte) {
				return acpRaw(t, acpRequest(t, http.MethodPost, url,
					json.RawMessage(body), key))
			}
			resp, first := send(tc.body)
			require.Equal(t, tc.status, resp.StatusCode, "%s", first)
			require.Empty(t, resp.Header.Get("Idempotent-Replayed"))
			tc.change(t, id, first)

			for _, body := range []string{tc.body, tc.equivalent} {
				resp, again := send(body)
				assert.Equal(t, tc.status, resp.StatusCode, body)
				assert.Equal(t, "true",
					resp.Header.Get("Idempotent-Replayed"), body)
				assert.Equal(t, key, resp.Header.Get("Idempotency-Key"))
				assert.Equal(t, string(first), string(again), body)
			}
			for _, body := range []string{tc.other, tc.nullForAbsent} {
				resp, refusal := send(body)
				assert.Equal(t, http.StatusUnprocessableEntity,
					resp.StatusCode, body)
				assert.Contains(t, string(refusal),
					`"code":"idempotency_conflict"`, body)
				assert.Empty(t, resp.Header.Get("Idempotent-Replayed"))
			}
		})
	}
}

// A complete's key is read before its session: after a completion the
// key replays what it answered and refuses another body (§6.4), and a
// complete without a key is refused (§6.1), whatever the session is.
func TestACompleteIsAnsweredByItsKeyFirst(t *testing.T) {
	stack := startUCPGateway(t)
	base := stack.gw.URL + "/acp/checkout_sessions"
	ctx := context.Background()
	payment := map[string]any{"payment_data": map[string]any{}}
	complete := func(id string, body any, key string) (
		*http.Response, []byte,
	) {
		return acpRaw(t, acpRequest(t, http.MethodPost,
			base+"/"+id+"/complete", body, key))
	}

	completed := acpReady(t, base)
	key := uuid.NewString()
	resp, first := complete(completed, payment, key)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", first)
	creates := stack.faults.orderCreates.Load()

	resp, again := complete(completed, payment, key)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "true", resp.Header.Get("Idempotent-Replayed"))
	assert.Equal(t, string(first), string(again))
	resp, refusal := complete(completed, map[string]any{
		"payment_data": map[string]any{},
		"buyer":        map[string]any{"email": "other@example.test"},
	}, key)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, string(refusal), `"code":"idempotency_conflict"`)

	resp, created := acpCall(t, http.MethodPost, base, map[string]any{
		"line_items":   []map[string]any{{"id": "1", "quantity": 2}},
		"currency":     "EUR",
		"capabilities": map[string]any{},
	}, uuid.NewString())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	canceled := created["id"].(string)
	resp, _ = acpCall(t, http.MethodPost, base+"/"+canceled+"/cancel", nil,
		uuid.NewString())
	require.Equal(t, http.StatusOK, resp.StatusCode)

	inProgress := acpReady(t, base)
	stored := stack.storedSession(t, inProgress)
	stored.Status = checkout.StatusCompleteInProgress
	stored.UpdatedAt = time.Now()
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	require.NoError(t, stack.rdb.Set(ctx, sessionRedisKey(inProgress), raw,
		redis.KeepTTL).Err())

	for name, id := range map[string]string{
		"completed": completed, "canceled": canceled,
		"in progress": inProgress,
	} {
		resp, body := complete(id, payment, "")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, name)
		assert.Contains(t, string(body),
			`"code":"idempotency_key_required"`, name)
	}
	assert.Equal(t, creates, stack.faults.orderCreates.Load())
}

// A request that did nothing, and one whose answer is a 5xx ("MUST NOT be
// cached against the idempotency key", §6.5), leave the key free: the
// retry with that key runs afresh.
func TestAnACPKeyIsFreedByWhatItDidNotDo(t *testing.T) {
	stack := startUCPGateway(t)
	base := stack.gw.URL + "/acp/checkout_sessions"
	ctx := context.Background()
	update := func(id, key string) (*http.Response, map[string]any) {
		return acpCall(t, http.MethodPost, base+"/"+id,
			map[string]any{"buyer": map[string]any{
				"email": "other@example.test",
			}}, key)
	}
	assertFreeThenKept := func(t *testing.T, id, key string) {
		t.Helper()
		n, err := stack.rdb.Exists(ctx,
			"ag:demostore:acp:idem:update.v3:"+id+":"+key).Result()
		require.NoError(t, err)
		assert.Zero(t, n, "the key is kept")
		resp, body := update(id, key)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "%v", body)
		assert.Empty(t, resp.Header.Get("Idempotent-Replayed"))
		resp, _ = update(id, key)
		assert.Equal(t, "true", resp.Header.Get("Idempotent-Replayed"))
	}

	t.Run("refused by the store", func(t *testing.T) {
		id := acpReady(t, base)
		key := uuid.NewString()
		stack.faults.payWayLookup.Store(true)
		t.Cleanup(func() { stack.faults.payWayLookup.Store(false) })
		resp, body := update(id, key)
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, "upstream_unavailable", body["code"])
		stack.faults.payWayLookup.Store(false)
		assertFreeThenKept(t, id, key)
	})

	// The update was saved, and its answer could not be rendered.
	t.Run("stored, answered with a 5xx", func(t *testing.T) {
		id := acpReady(t, base)
		key := uuid.NewString()
		stack.faults.namedCartReadsFailFrom.Store(
			stack.faults.namedCartReads.Load() + 1)
		t.Cleanup(func() { stack.faults.namedCartReadsFailFrom.Store(0) })
		resp, body := update(id, key)
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, "unavailable", body["code"])
		require.Equal(t, "other@example.test",
			stack.storedSession(t, id).Buyer.Email)
		stack.faults.namedCartReadsFailFrom.Store(0)
		assertFreeThenKept(t, id, key)
	})
}
