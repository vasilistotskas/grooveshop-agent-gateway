package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

var (
	// ErrNotFound marks an unknown or expired checkout session.
	ErrNotFound = errors.New("checkout: session not found")
	// ErrLocked marks a session busy with a concurrent mutation.
	ErrLocked = errors.New("checkout: session is busy, retry shortly")
	// ErrUnavailable marks checkout records that could not be read or
	// written: Redis failed, or holds what cannot be decoded. It is the
	// gateway's own store, not the shop's backend, and callers say so.
	ErrUnavailable = errors.New("checkout: records unavailable")
	// ErrSessionChanged marks a save that Redis refused: the stored
	// session is no longer the one this request read (another request
	// stored it since, or it expired). Nothing was written, and what the
	// request holds is stale.
	ErrSessionChanged = errors.New(
		"checkout: this checkout was changed by another request while " +
			"this one ran; read it again before retrying")
	// ErrCompletionInProgress marks a duplicate complete while the first
	// is still running.
	ErrCompletionInProgress = errors.New(
		"checkout: a completion of this checkout is in progress or " +
			"unconfirmed; check its status shortly instead of starting " +
			"another")
)

const (
	activeTTL   = 30 * time.Minute
	terminalTTL = 24 * time.Hour
	// The order link outlives the session it points at, and by a long
	// way: the UCP order object requires `checkout_id` for
	// reconciliation, and the store keeps sending order events (shipped,
	// delivered) long after the session's 24h. The link must survive for
	// as long as an agent might reasonably ask about an order. It is a
	// few short strings per order, so retention costs almost nothing.
	orderLinkTTL = 365 * 24 * time.Hour

	// lockSteps sizes the session lock, in upstream timeouts: the calls
	// a completion makes under it before its last write of the session.
	// The handler looks the payment up, Flow.admit reads the pay ways
	// and reserves the stock, and then the completion's own budget runs.
	// Django cuts each call at one timeout, so a request that holds the
	// lock longer has had a call retried (a GET, after a 5xx or a
	// dropped connection) or writes many cart lines. Its lock lapses
	// then, which costs nothing but the queueing: Save is what keeps a
	// session from being written by two requests.
	lockSteps = 3 + completionSteps

	// claimDone is the value of a completion key whose order was placed,
	// kept for the session's terminal lifetime so a retried key renders
	// the final state instead of placing a second order.
	claimDone = "done"
)

// Store persists sessions in Redis. Checkout fails closed without Redis:
// correctness beats availability on the money path.
type Store struct {
	rdb *redis.Client
	// timeout is the Django client's, which sizes the lock and the
	// claims and bounds a lock's release.
	timeout  time.Duration
	lockTTL  time.Duration
	claimTTL time.Duration
}

// NewStore sizes the lock and the idempotency claims from the Django
// client's timeout: a claim covers what the lock covers and the write
// that settles it afterwards.
func NewStore(rdb *redis.Client, upstreamTimeout time.Duration) *Store {
	lockTTL := lockSteps * upstreamTimeout
	return &Store{
		rdb: rdb, timeout: upstreamTimeout,
		lockTTL: lockTTL, claimTTL: lockTTL + upstreamTimeout,
	}
}

// sessionKey is versioned with how sessions are written. v2 sessions
// carry a revision and are stored only by compare-and-set; a pod that
// writes them unconditionally (v1) must never share one with a pod that
// does not, so neither reads the other's.
func sessionKey(schema, id string) string {
	return "ag:" + schema + ":cs:v2:" + id
}

func lockKey(schema, id string) string {
	return sessionKey(schema, id) + ":lock"
}

func idemKey(schema, id, key string) string {
	return "ag:" + schema + ":idem:" + id + ":" + key
}

func orderLinkKey(schema, orderUUID string) string {
	return "ag:" + schema + ":orderlink:" + orderUUID
}

func NewSession(schema, domain, protocol, cartID string) *Session {
	now := time.Now().UTC()
	return &Session{
		ID:        uuid.NewString(),
		Protocol:  protocol,
		Schema:    schema,
		Domain:    domain,
		Status:    StatusIncomplete,
		CartID:    cartID,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func (st *Store) Load(ctx context.Context, schema, id string) (*Session, error) {
	if err := uuid.Validate(id); err != nil {
		return nil, ErrNotFound
	}
	raw, err := st.rdb.Get(ctx, sessionKey(schema, id)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: load: %w", ErrUnavailable, err)
	}
	var s Session
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("%w: corrupt session: %w", ErrUnavailable, err)
	}
	return &s, nil
}

// saveScript stores the session ARGV[3], whose revision is ARGV[2], in
// KEYS[1] for ARGV[4] ms while the stored session's revision is still
// ARGV[1], and returns 1. No stored session counts as revision "",
// which only a session never stored has. Otherwise it writes nothing
// and returns 0. A lifetime of 0 or less is a placed session whose day
// has passed: it is deleted, under the same check. go-redis sends a
// command again when its reply was lost: the second run finds the
// revision the first one stored, which no other save can have written
// (or, after a delete, no session), and reports it done.
var saveScript = redis.NewScript(`
local stored = redis.call('GET', KEYS[1])
local ended = tonumber(ARGV[4]) <= 0
local revision = ''
if stored then
  revision = cjson.decode(stored).revision
end
if revision == ARGV[2] or (ended and not stored) then
  return 1
end
if revision ~= ARGV[1] then
  return 0
end
if ended then
  redis.call('DEL', KEYS[1])
else
  redis.call('SET', KEYS[1], ARGV[3], 'PX', ARGV[4])
end
return 1
`)

// Create stores a session that was never stored.
func (st *Store) Create(ctx context.Context, s *Session) error {
	return st.put(ctx, s, "", newWrite())
}

// write names one stored state of a session: its revision and the time
// it was stored. Sending one write twice is one write: a second run
// finds its revision stored and reports it done (saveScript).
type write struct {
	revision string
	at       time.Time
}

func newWrite() write {
	return write{revision: uuid.NewString(), at: time.Now().UTC()}
}

// Save stores s over the stored session it was loaded as, and only over
// that one: if another request has stored the session since, or it is
// gone, nothing is written, s is left as it was and the error is
// ErrSessionChanged. Whether the caller still holds the session lock
// plays no part.
func (st *Store) Save(ctx context.Context, s *Session) error {
	return st.put(ctx, s, s.Revision, newWrite())
}

// put stores s as w, while stored is the revision Redis holds.
func (st *Store) put(
	ctx context.Context, s *Session, stored string, w write,
) error {
	next := *s
	next.Revision = w.revision
	next.UpdatedAt = w.at
	key := sessionKey(s.Schema, s.ID)
	// Zero or less ends a placed session whose day has passed: the
	// script deletes it, under the same check as a write.
	ttl := next.ttl(next.UpdatedAt).Milliseconds()
	raw, err := json.Marshal(&next)
	if err != nil {
		return fmt.Errorf("checkout: encode: %w", err)
	}
	written, err := saveScript.Run(ctx, st.rdb, []string{key},
		stored, next.Revision, raw, ttl).Int()
	if err != nil {
		return fmt.Errorf("%w: save: %w", ErrUnavailable, err)
	}
	if written != 1 {
		return ErrSessionChanged
	}
	s.Revision, s.UpdatedAt = next.Revision, next.UpdatedAt
	return nil
}

// Current reports whether s is still the stored session: nil, or
// ErrSessionChanged when another request stored the session since s was
// loaded, or it is gone. It proves nothing about a moment later, which
// only Save does: it is for what a request does elsewhere on a session's
// behalf (a cart write in Django), so that it does none for a session it
// can already see has moved on.
func (st *Store) Current(ctx context.Context, s *Session) error {
	stored, err := st.Load(ctx, s.Schema, s.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return ErrSessionChanged
	case err != nil:
		return err
	case stored.Revision != s.Revision:
		return ErrSessionChanged
	}
	return nil
}

// Discard deletes a session that was created and never answered.
func (st *Store) Discard(ctx context.Context, s *Session) error {
	if err := st.rdb.Del(ctx, sessionKey(s.Schema, s.ID)).Err(); err != nil {
		return fmt.Errorf("%w: discard: %w", ErrUnavailable, err)
	}
	return nil
}

// Lock makes the requests on one session take turns: while one holds
// it, another gets ErrLocked. It orders them and protects nothing: the
// lock expires while its holder may still be working, and Save alone
// decides whether a session may be written. release frees the lock only
// while this call still holds it.
func (st *Store) Lock(ctx context.Context, schema, id string) (func(), error) {
	held, _, err := st.claim(ctx, lockKey(schema, id), st.lockTTL)
	if err != nil {
		return nil, fmt.Errorf("checkout: lock: %w", err)
	}
	if held == nil {
		return nil, ErrLocked
	}
	return func() {
		// Best effort, on a context the request cannot end: expiry
		// covers a release that fails.
		ctx, cancel := context.WithTimeout(context.Background(), st.timeout)
		defer cancel()
		_ = held.Release(ctx)
	}, nil
}

// ClaimCompletion makes complete idempotent: the first caller for a key
// gets its claim and places the order; a caller whose key has placed an
// order gets no claim and renders the session's current state; a caller
// whose key another attempt still holds gets ErrCompletionInProgress.
func (st *Store) ClaimCompletion(
	ctx context.Context, schema, id, idempotencyKey string,
) (*Claim, error) {
	claim, settled, err := st.Claim(ctx,
		idemKey(schema, id, idempotencyKey))
	switch {
	case err != nil:
		return nil, fmt.Errorf("checkout: idempotency: %w", err)
	case claim == nil && settled != claimDone:
		return nil, ErrCompletionInProgress
	}
	return claim, nil
}

// MarkCompleted spends a completion claim after its order was placed,
// and reports whether the key is spent.
func (st *Store) MarkCompleted(ctx context.Context, c *Claim) (bool, error) {
	return c.Settle(ctx, claimDone, terminalTTL)
}

// OrderLink ties an order placed through a checkout to what outlives
// that checkout: its id, the storefront domain the order belongs to, and
// the platform's webhook target.
type OrderLink struct {
	CheckoutID string `json:"checkoutId"`
	Domain     string `json:"domain"`
	WebhookURL string `json:"webhookUrl,omitempty"`
}

// placedOrder is the order a completion placed, recorded beside its
// session the moment Django returned it. The session itself learns of
// the order only with the completion's last save, which can fail: this
// record is then how later calls and order events know the order.
type placedOrder struct {
	OrderID   int64     `json:"orderId"`
	OrderUUID string    `json:"orderUuid"`
	PlacedAt  time.Time `json:"placedAt"`
}

func placedKey(schema, id string) string {
	return sessionKey(schema, id) + ":order"
}

// IndexOrder records the order the session's completion placed, both
// ways and together: the order's link to its checkout, and the placed
// order beside the session, kept as long as a placed session is.
func (st *Store) IndexOrder(ctx context.Context, s *Session) error {
	link, err := json.Marshal(OrderLink{
		CheckoutID: s.ID, Domain: s.Domain, WebhookURL: s.WebhookURL,
	})
	if err != nil {
		return fmt.Errorf("checkout: encode order link: %w", err)
	}
	placed, err := json.Marshal(placedOrder{
		OrderID: s.OrderID, OrderUUID: s.OrderUUID, PlacedAt: s.PlacedAt,
	})
	if err != nil {
		return fmt.Errorf("checkout: encode placed order: %w", err)
	}
	// The placed order first: an event finds the session through the
	// link, and must then find the order beside it.
	if err := st.rdb.Set(ctx, placedKey(s.Schema, s.ID), placed,
		terminalTTL).Err(); err != nil {
		return err
	}
	return st.rdb.Set(ctx, orderLinkKey(s.Schema, s.OrderUUID), link,
		orderLinkTTL).Err()
}

// placedOrder reads the order recorded beside a session; the zero value
// when none is.
func (st *Store) placedOrder(
	ctx context.Context, schema, id string,
) (placedOrder, error) {
	var placed placedOrder
	raw, err := st.rdb.Get(ctx, placedKey(schema, id)).Result()
	if errors.Is(err, redis.Nil) {
		return placed, nil
	}
	if err != nil {
		return placed, fmt.Errorf("%w: placed order: %w", ErrUnavailable, err)
	}
	if err := json.Unmarshal([]byte(raw), &placed); err != nil {
		return placed, fmt.Errorf("%w: corrupt placed order: %w",
			ErrUnavailable, err)
	}
	return placed, nil
}

// OrderLinkFor resolves the link for an order, without loading the
// session: the session expires long before the link does, so neither
// get_order nor order-event delivery may require it.
func (st *Store) OrderLinkFor(
	ctx context.Context, schema, orderUUID string,
) (*OrderLink, error) {
	raw, err := st.rdb.Get(ctx, orderLinkKey(schema, orderUUID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("checkout: order link: %w", err)
	}
	var link OrderLink
	if err := json.Unmarshal([]byte(raw), &link); err != nil {
		return nil, fmt.Errorf("checkout: corrupt order link: %w", err)
	}
	return &link, nil
}
