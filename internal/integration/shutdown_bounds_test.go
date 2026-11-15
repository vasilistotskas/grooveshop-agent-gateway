//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
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

// redisAt wraps each Redis command with the context it runs on: it
// decides whether the command is sent, and what its caller is told.
type redisAt func(
	ctx context.Context, cmd redis.Cmder, send func() error,
) error

func (redisAt) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (at redisAt) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		return at(ctx, cmd, func() error { return next(ctx, cmd) })
	}
}

func (redisAt) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return next
}

// pauseRedis stops the Redis behind rdb from answering any client for
// d, as a Redis that has gone silent does. It is asked on a connection
// of its own, outside rdb's hooks.
func pauseRedis(t *testing.T, rdb *redis.Client, d time.Duration) error {
	t.Helper()
	pauser := redis.NewClient(&redis.Options{Addr: rdb.Options().Addr})
	defer func() { _ = pauser.Close() }()
	return pauser.ClientPause(context.Background(), d).Err()
}

// A command a silent Redis does not answer ends at its context's
// deadline, not at the client's read timeout (config.RedisOptions): what
// the shutdown window assumes of every Redis step after a completion
// detaches.
func TestARedisCommandEndsAtItsContextDeadline(t *testing.T) {
	rdb := startRedis(t)
	require.NoError(t, pauseRedis(t, rdb, 2*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(),
		200*time.Millisecond)
	defer cancel()

	began := time.Now()
	err := rdb.Get(ctx, "ag:demostore:anything").Err()
	took := time.Since(began)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, took, time.Second,
		"the command outlived its deadline by %s", took-200*time.Millisecond)
}

// What a completion's request does on its own once the order is saved
// (settle its key, release its lock) is what the shutdown window holds
// for it: each step on a context of its own, bounded by one upstream
// timeout, and no more steps than checkout.CompletionTail counts. A
// step added after Complete, or one bounded by more, fails here.
func TestACompletionsStepsAfterItsOrderFitTheShutdownWindow(t *testing.T) {
	stack := startUCPGateway(t)
	var (
		watched atomic.Pointer[string]
		mu      sync.Mutex
		saved   bool
		steps   map[<-chan struct{}]time.Duration
	)
	stack.rdb.AddHook(redisAt(
		func(ctx context.Context, cmd redis.Cmder, send func() error) error {
			id := watched.Load()
			if id == nil || !strings.Contains(fmt.Sprint(cmd.Args()), *id) {
				return send()
			}
			mu.Lock()
			if deadline, ok := ctx.Deadline(); ok && saved {
				steps[ctx.Done()] = max(steps[ctx.Done()],
					time.Until(deadline))
			}
			mu.Unlock()
			err := send()
			if writesStatus(cmd, *id, checkout.StatusCompleted) {
				mu.Lock()
				saved = true
				mu.Unlock()
			}
			return err
		}))
	tail := checkout.CompletionTail(ucpUpstreamTimeout) -
		checkout.CompletionBudget(ucpUpstreamTimeout)

	for _, surface := range completionSurfaces(t, stack) {
		t.Run(surface.name, func(t *testing.T) {
			id := surface.ready(t)
			mu.Lock()
			saved, steps = false, map[<-chan struct{}]time.Duration{}
			mu.Unlock()
			watched.Store(&id)
			defer watched.Store(nil)

			got := surface.answer(context.Background(), id,
				uuid.NewString(), nil)

			require.Equal(t, "completed", got.status, got.refusal)
			assert.Eventually(t, func() bool {
				n, err := stack.rdb.Exists(context.Background(),
					lockRedisKey(id)).Result()
				return err == nil && n == 0
			}, ucpUpstreamTimeout, 10*time.Millisecond,
				"the session lock was never released")
			mu.Lock()
			defer mu.Unlock()
			require.True(t, saved, "the placed session was never saved")
			require.NotEmpty(t, steps, "no step ran after the order")
			for _, left := range steps {
				assert.LessOrEqual(t, left, ucpUpstreamTimeout,
					"a step is bounded by more than one upstream timeout")
			}
			assert.LessOrEqual(t,
				time.Duration(len(steps))*ucpUpstreamTimeout, tail,
				"%d steps after the order; the window holds %s of them",
				len(steps), tail)
		})
	}
}

// writesStatus reports whether cmd stores session id in status.
func writesStatus(
	cmd redis.Cmder, id string, status checkout.Status,
) bool {
	if !savesKey(cmd, sessionRedisKey(id)) || len(cmd.Args()) < 8 {
		return false
	}
	raw, _ := cmd.Args()[7].([]byte)
	var written checkout.Session
	return json.Unmarshal(raw, &written) == nil && written.Status == status
}
