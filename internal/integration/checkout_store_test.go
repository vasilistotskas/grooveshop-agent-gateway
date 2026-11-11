//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// storeTimeout is the upstream timeout the store tests size their locks
// and claims from.
const storeTimeout = 5 * time.Second

func startStore(t *testing.T) (*checkout.Store, *redis.Client) {
	t.Helper()
	rdb := startRedis(t)
	return checkout.NewStore(rdb, storeTimeout), rdb
}

func newSession() *checkout.Session {
	return checkout.NewSession("demostore", "shop.test",
		checkout.ProtocolUCP, "cart")
}

// sentTwice, while on, makes Redis run each script twice and hands back
// the second reply: what a caller reads when the first reply was lost
// and go-redis sent the command again. A script Redis does not know yet
// (the error to EVALSHA that makes go-redis send EVAL) ran no time, and
// is not run again.
type sentTwice struct {
	on atomic.Bool
	// scripts counts the scripts that ran twice.
	scripts atomic.Int32
}

func (*sentTwice) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *sentTwice) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		script := cmd.Name() == "evalsha" || cmd.Name() == "eval"
		if !script || !h.on.Load() {
			return next(ctx, cmd)
		}
		if err := next(ctx, cmd); err != nil {
			return err
		}
		h.scripts.Add(1)
		return next(ctx, cmd)
	}
}

func (*sentTwice) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return next
}

// during runs one store call with its script sent twice.
func (h *sentTwice) during(t *testing.T, call func()) {
	t.Helper()
	before := h.scripts.Load()
	h.on.Store(true)
	call()
	h.on.Store(false)
	require.Equal(t, before+1, h.scripts.Load(),
		"the call's script must have run twice")
}

// A caller touching an unpaid placed checkout must not keep it alive: it reads
// its order past the limit on a guest's uuid.
func TestPlacedSessionCannotBeKeptAlive(t *testing.T) {
	ctx := context.Background()
	store, rdb := startStore(t)

	s := newSession()
	s.Status = checkout.StatusRequiresEscalation
	s.PlacedAt = time.Now().Add(-24*time.Hour + 5*time.Minute)
	require.NoError(t, store.Create(ctx, s))
	ttl, err := rdb.TTL(ctx, sessionRedisKey(s.ID)).Result()
	require.NoError(t, err)
	assert.Positive(t, ttl)
	assert.LessOrEqual(t, ttl, 5*time.Minute)

	s.PlacedAt = time.Now().Add(-24 * time.Hour)
	require.NoError(t, store.Save(ctx, s))
	_, err = store.Load(ctx, s.Schema, s.ID)
	assert.ErrorIs(t, err, checkout.ErrNotFound)
}

// A save writes only over the session its caller read. Two requests that
// read one session cannot both store it, whatever became of the lock:
// the second is refused, and its session is left as it was.
func TestSaveWritesOnlyOverTheSessionItRead(t *testing.T) {
	ctx := context.Background()
	store, rdb := startStore(t)
	load := func(t *testing.T, s *checkout.Session) *checkout.Session {
		t.Helper()
		got, err := store.Load(ctx, s.Schema, s.ID)
		require.NoError(t, err)
		return got
	}

	t.Run("the second of two saves is refused", func(t *testing.T) {
		s := newSession()
		require.NoError(t, store.Create(ctx, s))
		first, second := load(t, s), load(t, s)
		first.Buyer.Email = "first@example.test"
		require.NoError(t, store.Save(ctx, first))

		second.Buyer.Email = "second@example.test"
		stale := *second
		require.ErrorIs(t, store.Save(ctx, second),
			checkout.ErrSessionChanged)

		assert.Equal(t, stale, *second, "a refused save changes nothing")
		assert.Equal(t, "first@example.test", load(t, s).Buyer.Email)
		// The winner keeps writing, each save over its own last one.
		first.Buyer.Phone = "+306900000000"
		require.NoError(t, store.Save(ctx, first))
		assert.Equal(t, first.Version, load(t, s).Version)
	})

	// Saves in one clock tick, or stamped by two pods' clocks, would look
	// alike by time. The version tells them apart.
	t.Run("a session is told apart by its version, not its time",
		func(t *testing.T) {
			s := newSession()
			require.NoError(t, store.Create(ctx, s))
			stale := load(t, s)
			require.NoError(t, store.Save(ctx, load(t, s)))
			// The later save, stamped with the stale one's time.
			stored := load(t, s)
			require.NotEqual(t, stale.Version, stored.Version)
			stored.UpdatedAt = stale.UpdatedAt
			raw, err := json.Marshal(stored)
			require.NoError(t, err)
			require.NoError(t, rdb.Set(ctx, sessionRedisKey(s.ID), raw,
				redis.KeepTTL).Err())

			assert.ErrorIs(t, store.Save(ctx, stale),
				checkout.ErrSessionChanged)
		})

	t.Run("a session that is gone is not stored again", func(t *testing.T) {
		s := newSession()
		require.NoError(t, store.Create(ctx, s))
		require.NoError(t, rdb.Del(ctx, sessionRedisKey(s.ID)).Err())

		require.ErrorIs(t, store.Save(ctx, s), checkout.ErrSessionChanged)

		_, err := store.Load(ctx, s.Schema, s.ID)
		assert.ErrorIs(t, err, checkout.ErrNotFound)
	})

	t.Run("a stored session is not created again", func(t *testing.T) {
		s := newSession()
		require.NoError(t, store.Create(ctx, s))
		again := *s
		again.Buyer.Email = "again@example.test"

		require.ErrorIs(t, store.Create(ctx, &again),
			checkout.ErrSessionChanged)

		assert.Empty(t, load(t, s).Buyer.Email)
	})

	t.Run("a discarded session is gone", func(t *testing.T) {
		s := newSession()
		require.NoError(t, store.Create(ctx, s))
		require.NoError(t, store.Discard(ctx, s))
		_, err := store.Load(ctx, s.Schema, s.ID)
		assert.ErrorIs(t, err, checkout.ErrNotFound)
	})
}

// go-redis sends a command again when its reply was lost. The first run
// did the work, so the second must report it done: a save answered
// "changed", or a lock answered "busy", by its own first run would fail
// a request that held everything it needed.
func TestACommandSentTwiceIsAnsweredAsDone(t *testing.T) {
	ctx := context.Background()
	store, rdb := startStore(t)
	twice := new(sentTwice)
	rdb.AddHook(twice)

	t.Run("a save", func(t *testing.T) {
		s := newSession()
		var err error
		twice.during(t, func() { err = store.Create(ctx, s) })
		require.NoError(t, err, "create")
		s.Buyer.Email = "saved@example.test"
		twice.during(t, func() { err = store.Save(ctx, s) })
		require.NoError(t, err, "save")
		stored, err := store.Load(ctx, s.Schema, s.ID)
		require.NoError(t, err)
		assert.Equal(t, s.Version, stored.Version)
		assert.Equal(t, "saved@example.test", stored.Buyer.Email)
	})

	t.Run("a lock", func(t *testing.T) {
		s := newSession()
		var release func()
		var err error
		twice.during(t, func() {
			release, err = store.Lock(ctx, s.Schema, s.ID)
		})
		require.NoError(t, err)
		twice.during(t, release)
		held, err := rdb.Exists(ctx, lockRedisKey(s.ID)).Result()
		require.NoError(t, err)
		assert.Zero(t, held, "the lock was released")
	})

	t.Run("a claim", func(t *testing.T) {
		var claim *checkout.Claim
		var err error
		twice.during(t, func() {
			claim, _, err = store.Claim(ctx,
				"ag:demostore:acp:idem:test:claim-twice")
		})
		require.NoError(t, err)
		assert.NotNil(t, claim, "the claim's own first run held the key")
	})

	t.Run("a settled claim", func(t *testing.T) {
		claim, _, err := store.Claim(ctx,
			"ag:demostore:acp:idem:test:settle-twice")
		require.NoError(t, err)
		require.NotNil(t, claim)
		var recorded bool
		twice.during(t, func() {
			recorded, err = claim.Settle(ctx, "record", time.Minute)
		})
		require.NoError(t, err)
		assert.True(t, recorded)
	})
}

// The lock makes requests on one session take turns. It expires by
// itself, and a request that outlived it must not release the lock of
// the request that took it next.
func TestLock(t *testing.T) {
	ctx := context.Background()
	store, rdb := startStore(t)
	s := newSession()

	release, err := store.Lock(ctx, s.Schema, s.ID)
	require.NoError(t, err)
	_, err = store.Lock(ctx, s.Schema, s.ID)
	require.ErrorIs(t, err, checkout.ErrLocked)

	// Six upstream timeouts: what a completion does under it.
	ttl, err := rdb.PTTL(ctx, lockRedisKey(s.ID)).Result()
	require.NoError(t, err)
	assert.InDelta(t, (6 * storeTimeout).Seconds(), ttl.Seconds(), 2)

	// The lock lapses and another request takes it.
	require.NoError(t, rdb.Del(ctx, lockRedisKey(s.ID)).Err())
	releaseNext, err := store.Lock(ctx, s.Schema, s.ID)
	require.NoError(t, err)
	release()
	_, err = store.Lock(ctx, s.Schema, s.ID)
	require.ErrorIs(t, err, checkout.ErrLocked,
		"the first holder released the next one's lock")

	releaseNext()
	again, err := store.Lock(ctx, s.Schema, s.ID)
	require.NoError(t, err, "a released lock is free")
	again()
}

// An idempotency key is held by one request at a time, and a request
// that ran past its claim neither settles nor releases the key of the
// request that holds it now.
func TestClaim(t *testing.T) {
	ctx := context.Background()
	store, rdb := startStore(t)
	const prefix = "ag:demostore:acp:idem:test:"
	// held reports who has key: a claim, a settled value, or neither.
	held := func(t *testing.T, key string) (bool, string) {
		t.Helper()
		claim, settled, err := store.Claim(ctx, key)
		require.NoError(t, err)
		return claim != nil, settled
	}

	t.Run("one request holds a key until it settles it", func(t *testing.T) {
		key := prefix + "settled"
		claim, _, err := store.Claim(ctx, key)
		require.NoError(t, err)
		require.NotNil(t, claim)
		ttl, err := rdb.PTTL(ctx, key).Result()
		require.NoError(t, err)
		assert.InDelta(t, (7 * storeTimeout).Seconds(), ttl.Seconds(), 2)

		claimed, settled := held(t, key)
		assert.False(t, claimed, "a second request claimed a held key")
		assert.Empty(t, settled)

		recorded, err := claim.Settle(ctx, "record", time.Hour)
		require.NoError(t, err)
		assert.True(t, recorded)
		claimed, settled = held(t, key)
		assert.False(t, claimed)
		assert.Equal(t, "record", settled)
		ttl, err = rdb.PTTL(ctx, key).Result()
		require.NoError(t, err)
		assert.InDelta(t, time.Hour.Seconds(), ttl.Seconds(), 2)
	})

	t.Run("a released key is free", func(t *testing.T) {
		key := prefix + "released"
		claim, _, err := store.Claim(ctx, key)
		require.NoError(t, err)
		require.NoError(t, claim.Release(ctx))
		claimed, _ := held(t, key)
		assert.True(t, claimed)
	})

	t.Run("a lapsed claim does not settle the next holder's key",
		func(t *testing.T) {
			key := prefix + "settle-lost"
			lapsed, _, err := store.Claim(ctx, key)
			require.NoError(t, err)
			require.NoError(t, rdb.Del(ctx, key).Err())
			holder, _, err := store.Claim(ctx, key)
			require.NoError(t, err)
			require.NotNil(t, holder)

			recorded, err := lapsed.Settle(ctx, "lapsed", time.Hour)
			require.NoError(t, err)
			assert.False(t, recorded)
			claimed, settled := held(t, key)
			assert.False(t, claimed)
			assert.Empty(t, settled, "the holder's claim was written over")

			// Nor its record, once the holder settled.
			recorded, err = holder.Settle(ctx, "holder", time.Hour)
			require.NoError(t, err)
			require.True(t, recorded)
			recorded, err = lapsed.Settle(ctx, "lapsed", time.Hour)
			require.NoError(t, err)
			assert.False(t, recorded)
			require.NoError(t, lapsed.Release(ctx))
			_, settled = held(t, key)
			assert.Equal(t, "holder", settled)
		})

	t.Run("a lapsed claim does not release the next holder's key",
		func(t *testing.T) {
			key := prefix + "release-lost"
			lapsed, _, err := store.Claim(ctx, key)
			require.NoError(t, err)
			require.NoError(t, rdb.Del(ctx, key).Err())
			holder, _, err := store.Claim(ctx, key)
			require.NoError(t, err)
			require.NotNil(t, holder)

			require.NoError(t, lapsed.Release(ctx))

			claimed, _ := held(t, key)
			assert.False(t, claimed, "the holder's claim was released")
		})

	// What the request did still stands, and no one else has the key:
	// it is settled, so the next request with it is a replay.
	t.Run("a lapsed claim no one took is still settled", func(t *testing.T) {
		key := prefix + "settle-late"
		lapsed, _, err := store.Claim(ctx, key)
		require.NoError(t, err)
		require.NoError(t, rdb.Del(ctx, key).Err())

		recorded, err := lapsed.Settle(ctx, "late", time.Hour)
		require.NoError(t, err)
		assert.True(t, recorded)
		_, settled := held(t, key)
		assert.Equal(t, "late", settled)
	})
}
