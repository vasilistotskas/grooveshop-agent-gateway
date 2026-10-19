package mcpsrv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// Turn names one chat turn, so the cart changes it makes are idempotent.
// The widget sends the same turn id when the shopper retries a failed
// turn, and the model repeats a call it saw fail: neither may add a
// product twice when the first attempt reached the store after all.
//
// A call's key is the turn, the call and how many identical calls this
// turn already completed. A repeat of a call whose outcome is unknown
// reuses its key, and Django answers it with the first outcome; a
// second identical call after a success is a new change, with a new key.
// Django scopes a key to its caller, which the gateway relays
// (django.RelayCaller), so a retry that reaches another pod is still
// the same caller's.
type Turn struct {
	id   string
	mu   sync.Mutex
	done map[string]int
}

func NewTurn(id string) *Turn {
	return &Turn{id: id, done: map[string]int{}}
}

type turnKey struct{}

// WithTurn has the cart tools called with ctx key their changes on t.
func WithTurn(ctx context.Context, t *Turn) context.Context {
	return context.WithValue(ctx, turnKey{}, t)
}

// idempotent returns ctx carrying call's idempotency key, and a function
// to record the call as completed. Without a turn (an /mcp agent) ctx
// is returned as is.
func idempotent(ctx context.Context, call string) (context.Context, func()) {
	t, ok := ctx.Value(turnKey{}).(*Turn)
	if !ok {
		return ctx, func() {}
	}
	t.mu.Lock()
	n := t.done[call]
	t.mu.Unlock()
	sum := sha256.Sum256([]byte(t.id + "\x00" + call + "\x00" +
		strconv.Itoa(n)))
	return django.WithIdempotencyKey(ctx, hex.EncodeToString(sum[:])),
		func() {
			t.mu.Lock()
			t.done[call]++
			t.mu.Unlock()
		}
}
