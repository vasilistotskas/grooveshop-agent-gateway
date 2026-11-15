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

func TestRedisOptionsRefuseABadURL(t *testing.T) {
	_, err := RedisOptions("http://redis-service:6379")

	assert.Error(t, err)
}
