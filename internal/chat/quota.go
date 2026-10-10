package chat

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Quota caps the turns a store's model key pays for: per store per clock
// hour, and per visitor (client IP) per store per UTC day. The counters
// live in Redis so every gateway pod spends from the same budget — the
// per-minute limiter in front of /chat is per pod and per IP, which a
// caller rotating addresses walks straight past.
type Quota struct {
	rdb           *redis.Client
	storePerHour  int
	visitorPerDay int
}

func NewQuota(rdb *redis.Client, storePerHour, visitorPerDay int) *Quota {
	return &Quota{rdb: rdb, storePerHour: storePerHour, visitorPerDay: visitorPerDay}
}

// takeTurn counts the turn against both windows only when both have room,
// so a refused turn spends nothing: a visitor locked out for the day does
// not also drain the store's hourly budget. KEYS: store, visitor counter.
// ARGV: store limit, visitor limit, store TTL (s), visitor TTL (s).
var takeTurn = redis.NewScript(`
local store = tonumber(redis.call('GET', KEYS[1]) or '0')
local visitor = tonumber(redis.call('GET', KEYS[2]) or '0')
if store >= tonumber(ARGV[1]) or visitor >= tonumber(ARGV[2]) then
  return 0
end
redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], ARGV[3], 'NX')
redis.call('INCR', KEYS[2])
redis.call('EXPIRE', KEYS[2], ARGV[4], 'NX')
return 1
`)

// Allow reports whether the store and the visitor both have a turn left,
// and spends one from each if so.
func (q *Quota) Allow(ctx context.Context, schema, visitor string, now time.Time) (bool, error) {
	now = now.UTC()
	hour := now.Truncate(time.Hour)
	day := now.Truncate(24 * time.Hour)
	prefix := "ag:" + schema + ":chat:quota:"
	keys := []string{
		prefix + "store:" + hour.Format("2006010215"),
		prefix + "visitor:" + visitor + ":" + day.Format("20060102"),
	}
	// Expire at the window's end, plus a minute so a key never outlives
	// its window by less than clock skew between pods and Redis.
	storeTTL := int(hour.Add(time.Hour).Sub(now).Seconds()) + 60
	visitorTTL := int(day.Add(24*time.Hour).Sub(now).Seconds()) + 60
	allowed, err := takeTurn.Run(ctx, q.rdb, keys,
		q.storePerHour, q.visitorPerDay, storeTTL, visitorTTL).Int()
	if err != nil {
		return false, err
	}
	return allowed == 1, nil
}
