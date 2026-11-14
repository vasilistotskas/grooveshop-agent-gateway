package config

import "github.com/redis/go-redis/v9"

// RedisOptions are the gateway's Redis client options for REDIS_URL.
//
// Every command ends at its context's deadline (ContextTimeoutEnabled):
// go-redis then sets each socket read and write deadline to the earlier
// of that deadline and its own read or write timeout, and starts no
// retry once the deadline has passed. Without it a command that starts
// just before its deadline runs on for up to the read timeout, which
// the shutdown window (cmd/gateway, completionTail) does not hold. A
// command cut there loses its connection, which the pool replaces.
func RedisOptions(rawURL string) (*redis.Options, error) {
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	opts.ContextTimeoutEnabled = true
	return opts, nil
}
