package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setRequired(t *testing.T) {
	t.Setenv("DJANGO_BASE_URL", "http://backend-service/api/v1/")
	t.Setenv("DJANGO_PUBLIC_HOST", "api.example.test")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("INTERNAL_EVENTS_SECRET", "s3cret")
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, ":8080", cfg.ListenAddr)
	assert.Equal(t, "production", cfg.Env)
	// Trailing slash on the base URL is normalized away.
	assert.Equal(t, "http://backend-service/api/v1", cfg.DjangoBaseURL)
	assert.Equal(t, 5*time.Minute, cfg.TenantCacheTTL)
	assert.Equal(t, time.Minute, cfg.NegativeCacheTTL)
	assert.Equal(t, 10*time.Second, cfg.UpstreamTimeout)
	assert.Equal(t, 120, cfg.RateLimitPerMin)
	assert.Equal(t, 40, cfg.RateLimitBurst)
}

func TestLoadMissingRequired(t *testing.T) {
	setRequired(t)
	t.Setenv("REDIS_URL", "")
	t.Setenv("INTERNAL_EVENTS_SECRET", "")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "REDIS_URL")
	assert.Contains(t, err.Error(), "INTERNAL_EVENTS_SECRET")
}

func TestLoadOverridesAndParsing(t *testing.T) {
	setRequired(t)
	t.Setenv("TENANT_CACHE_TTL", "30s")
	t.Setenv("RATE_LIMIT_PER_MIN", "600")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, cfg.TenantCacheTTL)
	assert.Equal(t, 600, cfg.RateLimitPerMin)
}

func TestLoadInvalidDuration(t *testing.T) {
	setRequired(t)
	t.Setenv("UPSTREAM_TIMEOUT", "not-a-duration")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UPSTREAM_TIMEOUT")
}
