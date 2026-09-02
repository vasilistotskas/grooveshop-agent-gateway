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
	t.Setenv("ASSETS_HOST", "assets.platform.test")
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, ":8080", cfg.ListenAddr)
	assert.Equal(t, EnvProduction, cfg.Env)
	assert.False(t, cfg.AllowLocalWebhooks,
		"production keeps the public-https webhook rule")
	assert.Equal(t, HandlerEnvProduction, cfg.PaymentHandlerEnv)
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

func TestLoadInvalidInt(t *testing.T) {
	setRequired(t)
	t.Setenv("CHAT_MAX_TURNS", "forty")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CHAT_MAX_TURNS")
}

// ASSETS_HOST is required, not defaulted: the gateway used to derive
// assets.<tenant-domain>, a hostname the documented onboarding never
// creates, and the resulting URLs went straight into product feeds and
// agent responses where nothing reports a broken image back.
func TestLoadRequiresAssetsHost(t *testing.T) {
	setRequired(t)
	t.Setenv("ASSETS_HOST", "")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ASSETS_HOST")
}

// ENV picks security-relevant behaviour, so an unrecognised value must
// refuse to boot rather than silently land on one side of it: the old
// .env.example shipped ENV=dev, which read as "not development" and
// kept the production webhook rule on a laptop. Only production may
// advertise the payment handler as production — a platform uses that
// to keep test traffic out of live order flow.
func TestLoadEnvIsValidated(t *testing.T) {
	setRequired(t)

	for _, env := range []string{EnvDevelopment, EnvTest} {
		t.Setenv("ENV", env)
		cfg, err := Load()
		require.NoError(t, err, env)
		assert.True(t, cfg.AllowLocalWebhooks, env)
		assert.Equal(t, HandlerEnvSandbox, cfg.PaymentHandlerEnv, env)
	}

	t.Setenv("ENV", "dev")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ENV")
}

func TestLoadLogLevelIsValidated(t *testing.T) {
	setRequired(t)
	t.Setenv("LOG_LEVEL", "verbose")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LOG_LEVEL")
}
