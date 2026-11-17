package config

import (
	"errors"

	"github.com/redis/go-redis/v9"
)

// errRedisURL is every REDIS_URL that does not parse.
var errRedisURL = errors.New("config: REDIS_URL is not a valid redis URL")

// RedisOptions are the gateway's Redis client options for REDIS_URL.
//
// Every command ends at its context's deadline (ContextTimeoutEnabled):
// go-redis then sets each socket read and write deadline to the earlier
// of that deadline and its own read or write timeout, and starts no
// retry once the deadline has passed. Without it a command that starts
// just before its deadline runs on for up to the read timeout, which
// the shutdown window (cmd/gateway, completionTail) does not hold. A
// command cut there loses its connection, which the pool replaces.
//
// A URL that does not parse is refused without its value and without
// go-redis's or net/url's reason, which repeat the URL or a piece of it:
// the URL carries the password.
func RedisOptions(rawURL string) (*redis.Options, error) {
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, errRedisURL
	}
	opts.ContextTimeoutEnabled = true
	return opts, nil
}
