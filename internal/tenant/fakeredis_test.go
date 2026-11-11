package tenant

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// fakeRedis is the shared Redis the unit tests give several resolvers (the
// pods of one deployment). It answers GET and SET itself, from a hook on the
// client, so no server and no extra dependency is involved; any other
// command fails the test. Keys expire by wall clock like Redis's own.
type fakeRedis struct {
	t  *testing.T
	mu sync.Mutex
	kv map[string]fakeEntry
}

type fakeEntry struct {
	val     string
	expires time.Time
}

func newFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	return &fakeRedis{t: t, kv: make(map[string]fakeEntry)}
}

// client returns a go-redis client backed by the fake. Every call to it is
// one pod's connection to the same store.
func (f *fakeRedis) client() *redis.Client {
	rdb := redis.NewClient(&redis.Options{
		// Never dialled: the hook answers every command it is given.
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			f.t.Error("fakeRedis: a command reached the network")
			return nil, net.ErrClosed
		},
	})
	rdb.AddHook(f)
	f.t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// raw returns the stored value of a live key.
func (f *fakeRedis) raw(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.kv[key]
	if !ok || time.Now().After(e.expires) {
		return "", false
	}
	return e.val, true
}

func (f *fakeRedis) DialHook(next redis.DialHook) redis.DialHook { return next }

func (f *fakeRedis) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return next
}

func (f *fakeRedis) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		switch cmd.Name() {
		case "get":
			c, ok := cmd.(*redis.StringCmd)
			key, isKey := args[1].(string)
			if !ok || !isKey {
				return f.refuse("GET of %T with key %T", cmd, args[1])
			}
			v, found := f.raw(key)
			if !found {
				// Client.Process sets the hook's return as the
				// command's error, so a miss is returned, not set.
				return redis.Nil
			}
			c.SetVal(v)
			return nil
		case "set":
			c, ok := cmd.(*redis.StatusCmd)
			if !ok || len(args) != 5 {
				return f.refuse("SET with %d arguments, want an expiry", len(args))
			}
			key, isKey := args[1].(string)
			n, isInt := args[4].(int64)
			val, isVal := valueOf(args[2])
			if !isKey || !isInt || !isVal {
				return f.refuse("SET arguments %T %T %T", args[1], args[2], args[4])
			}
			var ttl time.Duration
			switch args[3] {
			case "ex":
				ttl = time.Duration(n) * time.Second
			case "px":
				ttl = time.Duration(n) * time.Millisecond
			default:
				return f.refuse("SET option %v", args[3])
			}
			f.mu.Lock()
			f.kv[key] = fakeEntry{val: val, expires: time.Now().Add(ttl)}
			f.mu.Unlock()
			c.SetVal("OK")
			return nil
		}
		return f.refuse("command %q", cmd.Name())
	}
}

// refuse fails the test and the command. The hook runs on singleflight's
// goroutine, where t.Fatal would end the goroutine, the flight would never
// answer and the test would time out with this message lost.
func (f *fakeRedis) refuse(format string, args ...any) error {
	err := fmt.Errorf("fakeRedis: unsupported "+format, args...)
	f.t.Error(err)
	return err
}

// valueOf is the text of a SET value as the resolver passes it.
func valueOf(arg any) (string, bool) {
	switch v := arg.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	}
	return "", false
}

// newPod builds a resolver as another pod of the deployment: its own memory
// tier, the shared Redis and Django.
func newPod(t *testing.T, f *fakeDjango, rdb *redis.Client) *Resolver {
	t.Helper()
	return newPodLogging(t, f, rdb, testLogger())
}

func newPodLogging(
	t *testing.T, f *fakeDjango, rdb *redis.Client, log *slog.Logger,
) *Resolver {
	t.Helper()
	return NewResolver(f.client(t), rdb, testTTL, testNegTTL,
		log, nil)
}

// capturedLog is a logger that keeps its warnings and errors.
type capturedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capturedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturedLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *capturedLog) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(c, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))
}

// expireMemory ends the TTL of the memory entry for host without waiting
// for it. The entry must exist.
func expireMemory(t *testing.T, r *Resolver, host string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.mem[host]
	require.True(t, ok, "no memory entry for %s to expire", host)
	e.expires = time.Now().Add(-time.Second)
	r.mem[host] = e
}
