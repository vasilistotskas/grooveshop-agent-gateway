package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The gateway's client ends every command at its context's deadline, and
// keeps what REDIS_URL names.
func TestRedisOptionsEndCommandsAtTheirDeadline(t *testing.T) {
	opts, err := RedisOptions("redis://:secret@redis-service:6379/4")

	require.NoError(t, err)
	assert.True(t, opts.ContextTimeoutEnabled)
	assert.Equal(t, "redis-service:6379", opts.Addr)
	assert.Equal(t, 4, opts.DB)
	assert.Equal(t, "secret", opts.Password)
}

// A REDIS_URL that does not parse is refused without its value: the URL
// carries the password, and the error reaches the boot failure's line.
func TestRedisOptionsRefuseABadURLWithoutItsValue(t *testing.T) {
	for _, raw := range []string{
		"http://:s3cret@redis-service:6379",
		"redis://:s3cr%zzet@redis-service:6379/0",
		"redis://:s3cret@redis-service:6379/zero",
		"redis://:s3cret@redis-service:6379/0/1",
	} {
		_, err := RedisOptions(raw)

		require.ErrorIs(t, err, errRedisURL, raw)
		assert.NotContains(t, err.Error(), "s3cr", raw)
		assert.NotContains(t, err.Error(), "redis-service", raw)
	}
}
