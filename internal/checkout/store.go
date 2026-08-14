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
	// ErrCompletionInProgress marks a duplicate complete while the first
	// is still running.
	ErrCompletionInProgress = errors.New(
		"checkout: completion already in progress, retry shortly")
)

const (
	activeTTL   = 30 * time.Minute
	terminalTTL = 24 * time.Hour
	orderIdxTTL = 30 * 24 * time.Hour
	lockTTL     = 10 * time.Second
)

// Store persists sessions in Redis. Checkout fails closed without Redis:
// correctness beats availability on the money path.
type Store struct {
	rdb *redis.Client
}

func NewStore(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

func sessionKey(schema, id string) string {
	return "ag:" + schema + ":cs:" + id
}

func lockKey(schema, id string) string {
	return sessionKey(schema, id) + ":lock"
}

func idemKey(schema, id, key string) string {
	return "ag:" + schema + ":idem:" + id + ":" + key
}

func orderKey(schema, orderUUID string) string {
	return "ag:" + schema + ":order:" + orderUUID
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
		Version:   1,
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
		return nil, fmt.Errorf("checkout: load: %w", err)
	}
	var s Session
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("checkout: corrupt session: %w", err)
	}
	return &s, nil
}

func (st *Store) Save(ctx context.Context, s *Session) error {
	s.Version++
	s.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("checkout: encode: %w", err)
	}
	ttl := activeTTL
	if s.Terminal() {
		ttl = terminalTTL
	}
	if err := st.rdb.Set(ctx, sessionKey(s.Schema, s.ID), raw, ttl).Err(); err != nil {
		return fmt.Errorf("checkout: save: %w", err)
	}
	return nil
}

// Lock serializes mutations on one session. The returned release func is
// safe to call once; expiry covers crashed holders.
func (st *Store) Lock(ctx context.Context, schema, id string) (func(), error) {
	token := uuid.NewString()
	ok, err := st.rdb.SetNX(ctx, lockKey(schema, id), token, lockTTL).Result()
	if err != nil {
		return nil, fmt.Errorf("checkout: lock: %w", err)
	}
	if !ok {
		return nil, ErrLocked
	}
	return func() {
		// Best-effort compare-and-delete so an expired lock taken over
		// by another holder is not released from here.
		const script = `if redis.call("get", KEYS[1]) == ARGV[1] then
			return redis.call("del", KEYS[1]) else return 0 end`
		_ = st.rdb.Eval(context.Background(), script,
			[]string{lockKey(schema, id)}, token).Err()
	}, nil
}

// ClaimCompletion makes complete idempotent: the first caller claims the
// slot, duplicates read the stored outcome or are told to retry.
func (st *Store) ClaimCompletion(
	ctx context.Context, schema, id, idempotencyKey string,
) (prior []byte, claimed bool, err error) {
	key := idemKey(schema, id, idempotencyKey)
	ok, err := st.rdb.SetNX(ctx, key, "inflight", time.Minute).Result()
	if err != nil {
		return nil, false, fmt.Errorf("checkout: idempotency: %w", err)
	}
	if ok {
		return nil, true, nil
	}
	raw, err := st.rdb.Get(ctx, key).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, false, fmt.Errorf("checkout: idempotency read: %w", err)
	}
	if raw == "" || raw == "inflight" {
		return nil, false, ErrCompletionInProgress
	}
	return []byte(raw), false, nil
}

// StoreCompletion persists the final completion payload for duplicates.
func (st *Store) StoreCompletion(
	ctx context.Context, schema, id, idempotencyKey string, payload []byte,
) error {
	return st.rdb.Set(ctx, idemKey(schema, id, idempotencyKey),
		payload, terminalTTL).Err()
}

// ReleaseCompletion clears an inflight claim after a failed attempt so the
// caller can retry.
func (st *Store) ReleaseCompletion(
	ctx context.Context, schema, id, idempotencyKey string,
) {
	_ = st.rdb.Del(ctx, idemKey(schema, id, idempotencyKey)).Err()
}

// IndexOrder maps a Django order UUID to its session for webhook routing.
func (st *Store) IndexOrder(
	ctx context.Context, schema, orderUUID, sessionID string,
) error {
	return st.rdb.Set(ctx, orderKey(schema, orderUUID), sessionID,
		orderIdxTTL).Err()
}

// SessionForOrder resolves the session an order event belongs to.
func (st *Store) SessionForOrder(
	ctx context.Context, schema, orderUUID string,
) (*Session, error) {
	id, err := st.rdb.Get(ctx, orderKey(schema, orderUUID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("checkout: order index: %w", err)
	}
	return st.Load(ctx, schema, id)
}
