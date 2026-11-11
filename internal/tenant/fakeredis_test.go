package tenant

import (
	"context"
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
			c := cmd.(*redis.StringCmd)
			if v, ok := f.raw(args[1].(string)); ok {
				c.SetVal(v)
			} else {
				c.SetErr(redis.Nil)
			}
		case "set":
			c := cmd.(*redis.StatusCmd)
			require.Len(f.t, args, 5, "fakeRedis: SET with an expiry only")
			var ttl time.Duration
			switch args[3] {
			case "ex":
				ttl = time.Duration(args[4].(int64)) * time.Second
			case "px":
				ttl = time.Duration(args[4].(int64)) * time.Millisecond
			default:
				f.t.Fatalf("fakeRedis: unexpected SET option %v", args[3])
			}
			f.mu.Lock()
			f.kv[args[1].(string)] = fakeEntry{
				val:     valueOf(f.t, args[2]),
				expires: time.Now().Add(ttl),
			}
			f.mu.Unlock()
			c.SetVal("OK")
		default:
			f.t.Fatalf("fakeRedis: unexpected command %q", cmd.Name())
		}
		return nil
	}
}

// valueOf is the text of a SET value as the resolver passes it.
func valueOf(t *testing.T, arg any) string {
	t.Helper()
	switch v := arg.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	t.Fatalf("fakeRedis: unexpected SET value type %T", arg)
	return ""
}

// newPod builds a resolver as another pod of the deployment: its own memory
// tier, the shared Redis and Django.
func newPod(t *testing.T, f *fakeDjango, rdb *redis.Client) *Resolver {
	t.Helper()
	return NewResolver(f.client(t), rdb, time.Minute, 50*time.Millisecond,
		testLogger(), nil)
}

// expireMemory ends the TTL of the memory entry for secretsHost without
// waiting for it.
func expireMemory(r *Resolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.mem[secretsHost]
	e.expires = time.Now().Add(-time.Second)
	r.mem[secretsHost] = e
}
