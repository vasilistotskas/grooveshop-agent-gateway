package feeds

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func demoTenant() *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{SchemaName: "demostore"},
	}
}

// A panic in a generation comes back as an error, without the panic
// text or stack, instead of ending the process from singleflight's own
// goroutine.
func TestRefreshReturnsPanicAsError(t *testing.T) {
	rdb := unreachableRedis()
	rdb.AddHook(panicOnGet{})
	metrics := obs.NewMetrics()
	s := NewService(nil, rdb, slog.New(slog.DiscardHandler), metrics,
		"", "", time.Hour, 24*time.Hour, "test", storefront.NewSite("el"))
	res := <-s.refresh(context.Background(), demoTenant())
	require.EqualError(t, res.Err, "feeds: generation: panicked")
	assert.InDelta(t, 1, counterValue(t, metrics,
		"gateway_feed_generation_failures_total"), 0)
}

// panicOnGet panics on reading the epoch, the first thing a generation
// does; every other command goes on to fail against no server.
type panicOnGet struct{}

func (panicOnGet) DialHook(next redis.DialHook) redis.DialHook { return next }

func (panicOnGet) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" {
			panic("epoch read")
		}
		return next(ctx, cmd)
	}
}

func (panicOnGet) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return next
}

// scriptedRedis answers every command itself through answer, with no
// server, and records each command's arguments.
type scriptedRedis struct {
	mu     sync.Mutex
	seen   [][]any
	answer func(redis.Cmder) error
}

func (s *scriptedRedis) client() *redis.Client {
	rdb := unreachableRedis()
	rdb.AddHook(s)
	return rdb
}

func (s *scriptedRedis) run(cmd redis.Cmder) error {
	s.mu.Lock()
	s.seen = append(s.seen, cmd.Args())
	s.mu.Unlock()
	err := s.answer(cmd)
	cmd.SetErr(err)
	return err
}

// set reports whether a SET of key was sent.
func (s *scriptedRedis) set(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, args := range s.seen {
		if args[0] == "set" && len(args) > 1 && args[1] == key {
			return true
		}
	}
	return false
}

func (*scriptedRedis) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (s *scriptedRedis) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error { return s.run(cmd) }
}

func (s *scriptedRedis) ProcessPipelineHook(
	redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return func(_ context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if err := s.run(cmd); err != nil {
				return err
			}
		}
		return nil
	}
}

// A short MGET reply is a miss, never an index out of range on the
// request's goroutine.
func TestShortMGetReplyIsAMiss(t *testing.T) {
	r := &scriptedRedis{answer: func(cmd redis.Cmder) error {
		switch c := cmd.(type) {
		case *redis.SliceCmd:
			c.SetVal([]any{nil})
		case *redis.IntCmd:
			c.SetVal(1)
		}
		return nil
	}}
	s := NewService(nil, r.client(), slog.New(slog.DiscardHandler),
		obs.NewMetrics(), "", "", time.Hour, 24*time.Hour, "test",
		storefront.NewSite("el"))
	require.NotPanics(t, func() {
		_, ok, failed, err := s.cachedMeta(context.Background(),
			"demostore", formatRSS)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, failed)
	})
}

// A generation that fails before it reads the epoch (the epoch read
// itself, or the wait for a slot) is marked all the same: no
// invalidation can have overtaken it.
func TestFailureBeforeTheEpochIsMarked(t *testing.T) {
	r := &scriptedRedis{answer: func(cmd redis.Cmder) error {
		if cmd.Name() == "get" {
			return errors.New("epoch unreadable")
		}
		return nil
	}}
	metrics := obs.NewMetrics()
	s := NewService(nil, r.client(), slog.New(slog.DiscardHandler),
		metrics, "", "", time.Hour, 24*time.Hour, "test",
		storefront.NewSite("el"))
	res := <-s.refresh(context.Background(), demoTenant())
	require.ErrorContains(t, res.Err, "epoch unreadable")
	assert.True(t, r.set("ag:demostore:feed:failed"))
}

// unreachableRedis fails every command at once.
func unreachableRedis() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 20 * time.Millisecond,
		MaxRetries:  -1,
		PoolSize:    1,
	})
}

// A failed generation is logged once, under the request that started
// it (even after that request has gone), and counted.
func TestRefreshFailureIsLoggedAndCounted(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(obs.NewContextHandler(slog.NewJSONHandler(&logs, nil)))
	metrics := obs.NewMetrics()
	s := NewService(nil, unreachableRedis(), log, metrics, "", "",
		time.Hour, 24*time.Hour, "test", storefront.NewSite("el"))

	ctx := obs.WithRequestID(
		obs.WithTenantSlot(context.Background()), "req-98")
	obs.SetTenant(ctx, "demostore")
	ctx, cancel := context.WithCancel(ctx)
	cancel()

	res := <-s.refresh(ctx, demoTenant())
	require.Error(t, res.Err)
	assert.NotErrorIs(t, res.Err, context.Canceled,
		"the generation is detached from its request")

	var failed map[string]any
	sc := bufio.NewScanner(&logs)
	for sc.Scan() {
		var line map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line))
		if line["msg"] == "feed generation failed" {
			require.Nil(t, failed, "logged once")
			failed = line
		}
	}
	require.NotNil(t, failed, logs.String())
	assert.Equal(t, "ERROR", failed["level"])
	assert.Equal(t, "req-98", failed[obs.KeyRequestID])
	assert.Equal(t, "demostore", failed[obs.KeyTenant])
	assert.Equal(t, res.Err.Error(), failed["error"])

	assert.InDelta(t, 1, counterValue(t, metrics,
		"gateway_feed_generation_failures_total"), 0)
}

func counterValue(t *testing.T, m *obs.Metrics, name string) float64 {
	t.Helper()
	families, err := m.Registry.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() == name {
			require.Len(t, f.GetMetric(), 1)
			return f.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("metric %s not registered", name)
	return 0
}
