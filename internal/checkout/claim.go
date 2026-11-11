package checkout

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// claimPrefix starts the value of a key a request holds; the request's
// own token follows. A settled key holds anything else.
const claimPrefix = "inflight:"

// claimScript gives KEYS[1] to the marker ARGV[1] for ARGV[2] ms when
// nothing holds the key, and returns what the key holds then. go-redis
// sends a command again when its reply was lost: the second run finds
// the first one's marker, which its caller knows as its own.
var claimScript = redis.NewScript(`
redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2])
return redis.call('GET', KEYS[1])
`)

// settleScript puts ARGV[2] in KEYS[1] for ARGV[3] ms while the key
// holds the marker ARGV[1], or nothing: a claim that lapsed with no one
// after it. It returns 1 when the key holds ARGV[2], from this run or
// from one whose reply was lost, and 0 when the key is another
// request's.
var settleScript = redis.NewScript(`
local held = redis.call('GET', KEYS[1])
if held == ARGV[2] then
  return 1
end
if held and held ~= ARGV[1] then
  return 0
end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
return 1
`)

// releaseScript deletes KEYS[1] while it holds the marker ARGV[1].
var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// Claim is a Redis key one request holds while it works: a session's
// lock, or an idempotency key. The claim expires by itself, so its
// request can outlive it and another request can hold the key by then.
// Settle and Release therefore act only on a key that is still this
// claim's, never on the next holder's.
type Claim struct {
	rdb    *redis.Client
	key    string
	marker string
}

// Claim takes an idempotency key for one request. It returns the claim
// when the request holds the key, the value the key was settled with
// when an earlier request finished with it, and neither while another
// request holds it.
func (st *Store) Claim(
	ctx context.Context, key string,
) (claim *Claim, settled string, err error) {
	return st.claim(ctx, key, st.claimTTL)
}

func (st *Store) claim(
	ctx context.Context, key string, ttl time.Duration,
) (*Claim, string, error) {
	c := &Claim{
		rdb: st.rdb, key: key, marker: claimPrefix + uuid.NewString(),
	}
	held, err := claimScript.Run(ctx, st.rdb, []string{key},
		c.marker, ttl.Milliseconds()).Text()
	switch {
	case err != nil:
		return nil, "", err
	case held == c.marker:
		return c, "", nil
	case strings.HasPrefix(held, claimPrefix):
		return nil, "", nil
	}
	return nil, held, nil
}

// Settle replaces the claim with value, kept for ttl, and reports
// whether the key holds it. It does not when the claim lapsed and
// another request has the key: that request settles it.
func (c *Claim) Settle(
	ctx context.Context, value string, ttl time.Duration,
) (bool, error) {
	n, err := settleScript.Run(ctx, c.rdb, []string{c.key},
		c.marker, value, ttl.Milliseconds()).Int()
	return n == 1, err
}

// Release frees the key for the next request, if the claim still holds
// it.
func (c *Claim) Release(ctx context.Context) error {
	return releaseScript.Run(ctx, c.rdb, []string{c.key}, c.marker).Err()
}
