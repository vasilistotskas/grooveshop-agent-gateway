//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// awaitKEK runs the boot check of keys to its outcome.
func awaitKEK(t *testing.T, keys *ucp.Keys) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return keys.AwaitKEK(ctx, quietLogger())
}

// A fresh environment, with no signing key stored, passes the boot
// check: there is nothing to check the key-encryption key against.
func TestAwaitKEKPassesAnEnvironmentWithNoKeys(t *testing.T) {
	rdb := startRedis(t)
	keys := newKeys(t, rdb, testKEK(t))
	require.NoError(t, awaitKEK(t, keys))
	require.NoError(t, keys.Checked())
}

// A key-encryption key that opens none of the stored keys (another
// environment's) is the definite failure; the right one passes.
func TestAwaitKEKRefusesAKeyThatOpensNone(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	for _, schema := range []string{"alpha", "beta"} {
		_, err := newKeys(t, rdb, kek).ForSchema(ctx, schema)
		require.NoError(t, err)
	}

	wrong := newKeys(t, rdb, testKEK(t))
	require.ErrorIs(t, awaitKEK(t, wrong), ucp.ErrKEKOpensNoKey)
	require.Error(t, wrong.Checked(), "never ready")
	right := newKeys(t, rdb, kek)
	require.NoError(t, awaitKEK(t, right))
	require.NoError(t, right.Checked())
}

// One record that does not open (copied from another store) beside keys
// that do is that store's fault, not the key-encryption key's: the boot
// goes on.
func TestAwaitKEKPassesWhenAKeyOpens(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	_, err := newKeys(t, rdb, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	sealed, err := rdb.Get(ctx, sealedKeyName("alpha")).Result()
	require.NoError(t, err)
	require.NoError(t, rdb.Set(ctx, sealedKeyName("beta"), sealed, 0).Err())

	require.NoError(t, awaitKEK(t, newKeys(t, rdb, kek)))
}

// failNthGet fails the nth GET once, as a connection lost mid-scan, and
// counts the SCANs that start over from cursor 0.
type failNthGet struct {
	n           int32
	gets        atomic.Int32
	scansFromNo atomic.Int32
}

func (h *failNthGet) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *failNthGet) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return next
}

func (h *failNthGet) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		switch cmd.Name() {
		case "scan":
			if fmt.Sprint(cmd.Args()[1]) == "0" {
				h.scansFromNo.Add(1)
			}
		case "get":
			if h.gets.Add(1) == h.n {
				err := errors.New("connection reset by peer")
				cmd.SetErr(err)
				return err
			}
		}
		return next(ctx, cmd)
	}
}

// A scan cut short goes on from its cursor in the next attempt instead
// of starting over, so a scan of more records than one attempt reads
// still completes.
func TestAwaitKEKResumesItsScan(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	_, err := newKeys(t, rdb, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	// More records than one SCAN returns: the check reads them in
	// several batches.
	pipe := rdb.Pipeline()
	for i := range 3000 {
		pipe.Set(ctx, sealedKeyName(fmt.Sprintf("store%04d", i)), "junk", 0)
	}
	_, err = pipe.Exec(ctx)
	require.NoError(t, err)

	hook := &failNthGet{n: 2000}
	rdb.AddHook(hook)
	keys := newKeys(t, rdb, kek)
	require.NoError(t, awaitKEK(t, keys))
	require.NoError(t, keys.Checked())
	assert.GreaterOrEqual(t, hook.gets.Load(), int32(3001))
	assert.EqualValues(t, 1, hook.scansFromNo.Load(),
		"the scan goes on from its cursor after a failure")
}

// proxyTo serves addr by forwarding each connection to target, until the
// test ends: a Redis that becomes reachable at addr once it starts.
func proxyTo(t *testing.T, addr, target string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				_ = in.Close()
				continue
			}
			go func() { _, _ = io.Copy(out, in); _ = out.Close() }()
			go func() { _, _ = io.Copy(in, out); _ = in.Close() }()
		}
	}()
}

// A pod that boots while Redis does not answer waits for it, as its
// readiness does, and is not ready until the check passed: it never
// gives up, so it never crash-loops into the restart backoff. Once
// Redis answers, the check passes and the pod is ready.
func TestAwaitKEKWaitsForRedisAndThenPasses(t *testing.T) {
	upstream := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	_, err := newKeys(t, upstream, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)

	// An address nothing listens on yet.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	keys := newKeys(t, rdb, kek)

	awaitCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- keys.AwaitKEK(awaitCtx, quietLogger()) }()

	time.Sleep(1500 * time.Millisecond)
	require.Error(t, keys.Checked(), "not ready while Redis does not answer")
	select {
	case err := <-done:
		t.Fatalf("the check gave up while Redis was down: %v", err)
	default:
	}

	proxyTo(t, addr, upstream.Options().Addr)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("the check did not pass once Redis answered")
	}
	require.NoError(t, keys.Checked(), "ready once the check passed")
}

// The wait ends with its context: a shutdown stops it.
func TestAwaitKEKStopsWithItsContext(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1",
		MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	keys := newKeys(t, rdb, testKEK(t))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.ErrorIs(t, keys.AwaitKEK(ctx, quietLogger()),
		context.DeadlineExceeded)
	require.Error(t, keys.Checked())
}

// The sentinel a mint leaves expires by itself, so stores first keyed
// after the seal command ran leave nothing behind for good; the key
// itself never expires.
func TestTheSentinelExpires(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	_, err := newKeys(t, rdb, testKEK(t)).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	ttl, err := rdb.PTTL(ctx, unsealedKeyName("alpha")).Result()
	require.NoError(t, err)
	assert.Positive(t, ttl)
	assert.LessOrEqual(t, ttl, 7*24*time.Hour)
	keyTTL, err := rdb.PTTL(ctx, sealedKeyName("alpha")).Result()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(-1), keyTTL)
}
