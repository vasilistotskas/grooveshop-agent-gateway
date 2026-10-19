package mcpsrv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Turn names one chat turn, so an add it makes is made once. The widget
// sends the same turn id when the shopper retries a failed turn, and the
// first attempt may have added the product before the turn failed. The
// retry can reach another pod, so what the turn added is recorded in the
// Redis every pod shares. Django's Idempotency-Key cannot do this: it
// scopes a key to the caller's address, and the caller is one pod.
//
// A change's key is the turn, the call and how many identical calls this
// turn already made. A retried turn's first identical add finds the first
// attempt's record and is not sent again; a second identical add after a
// success is a new change, with a new key.
type Turn struct {
	rdb    *redis.Client
	schema string
	id     string
	// ttl keeps a made change for as long as its conversation can be
	// retried.
	ttl time.Duration

	mu   sync.Mutex
	made map[string]int
}

func NewTurn(
	rdb *redis.Client, schema, id string, ttl time.Duration,
) *Turn {
	return &Turn{
		rdb: rdb, schema: schema, id: id, ttl: ttl, made: map[string]int{},
	}
}

type turnKey struct{}

// WithTurn has the cart tools called with ctx record their adds on t.
func WithTurn(ctx context.Context, t *Turn) context.Context {
	return context.WithValue(ctx, turnKey{}, t)
}

const (
	changeInflight = "inflight"
	changeMade     = "made"
	// changeInflightTTL outlasts the Django call it guards
	// (UPSTREAM_TIMEOUT), and frees the claim of a pod that died
	// mid-call.
	changeInflightTTL = time.Minute
)

// errChangeInFlight: another attempt of the turn is making the change.
var errChangeInFlight = errors.New("mcpsrv: turn change in flight")

// change is one claimed cart change. Without a turn (an /mcp agent) it
// claims nothing and records nothing.
type change struct {
	turn *Turn
	call string
	key  string
	// made: an earlier attempt of the turn already made the change, so
	// it must not be sent again.
	made bool
}

// claimChange claims call for the turn in ctx: it returns the change to
// make, one already made, or errChangeInFlight.
func claimChange(ctx context.Context, call string) (*change, error) {
	t, ok := ctx.Value(turnKey{}).(*Turn)
	if !ok {
		return &change{}, nil
	}
	t.mu.Lock()
	n := t.made[call]
	t.mu.Unlock()
	sum := sha256.Sum256([]byte(t.id + "\x00" + call + "\x00" +
		strconv.Itoa(n)))
	c := &change{
		turn: t, call: call,
		key: "ag:" + t.schema + ":chat:change:" + hex.EncodeToString(sum[:]),
	}
	claimed, err := t.rdb.SetNX(ctx, c.key, changeInflight,
		changeInflightTTL).Result()
	if err != nil {
		return nil, err
	}
	if claimed {
		return c, nil
	}
	// A claim that expired between the two calls is treated as still in
	// flight: the store may have made it.
	state, err := t.rdb.Get(ctx, c.key).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	if state != changeMade {
		return nil, errChangeInFlight
	}
	c.made = true
	c.count()
	return c, nil
}

// done records the change as made, for a retry of the turn. It outlives
// the request: a shopper who leaves just after the store made the change
// is the one who retries it.
func (c *change) done(ctx context.Context) {
	if c.turn == nil {
		return
	}
	// Best effort: the store made the change either way, and a lost
	// record costs only the guard, once the in-flight claim expires.
	_ = c.turn.rdb.Set(context.WithoutCancel(ctx), c.key, changeMade,
		c.turn.ttl).Err()
	c.count()
}

// release frees the claim of a change the store refused, so the turn
// can make it again. A change whose outcome is unknown keeps its claim
// until it expires: the store may still be making it.
func (c *change) release(ctx context.Context) {
	if c.turn == nil {
		return
	}
	_ = c.turn.rdb.Del(context.WithoutCancel(ctx), c.key).Err()
}

func (c *change) count() {
	c.turn.mu.Lock()
	c.turn.made[c.call]++
	c.turn.mu.Unlock()
}
