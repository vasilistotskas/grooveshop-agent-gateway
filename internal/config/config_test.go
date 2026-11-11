package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setRequired(t *testing.T) {
	t.Setenv("DJANGO_BASE_URL", "http://backend-service/api/v1/")
	t.Setenv("DJANGO_PUBLIC_HOST", "api.example.test")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("INTERNAL_EVENTS_SECRET", strings.Repeat("a", minInternalSecretLen))
	t.Setenv("ASSETS_HOST", "assets.platform.test")
	t.Setenv("STAFF_CONNECT_HOST", "connect.platform.test")
	t.Setenv("STOREFRONT_DEFAULT_LOCALE", "el")
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
	assert.Equal(t, time.Minute, cfg.ChatTurnTimeout)
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
	t.Setenv("CHAT_TURN_TIMEOUT", "45s")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, cfg.TenantCacheTTL)
	assert.Equal(t, 600, cfg.RateLimitPerMin)
	assert.Equal(t, 45*time.Second, cfg.ChatTurnTimeout)
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

// Zero is never a working TTL, timeout or limit: TENANT_CACHE_TTL=0
// would write tenant config to Redis with no expiry.
func TestLoadRejectsNonPositiveValues(t *testing.T) {
	for key, v := range map[string]string{
		"TENANT_CACHE_TTL":          "0s",
		"TENANT_NEGATIVE_CACHE_TTL": "-1m",
		"UPSTREAM_TIMEOUT":          "0",
		"RATE_LIMIT_PER_MIN":        "0",
		"CHAT_MAX_ITERATIONS":       "-3",
		"CHAT_TURN_TIMEOUT":         "0s",
		"FEED_STALE_TTL":            "0h",
	} {
		t.Run(key, func(t *testing.T) {
			setRequired(t)
			t.Setenv(key, v)
			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), key+" must be positive")
		})
	}
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

// STAFF_CONNECT_HOST is required: /mcp/admin's protected-resource
// metadata names it as the authorization server, and a guessed host would
// send a store's staff to sign in somewhere that is not the platform's.
func TestLoadRequiresStaffConnectHost(t *testing.T) {
	setRequired(t)
	t.Setenv("STAFF_CONNECT_HOST", "")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "STAFF_CONNECT_HOST")
}

// The storefront's build default decides which links carry a language
// prefix, so it is required and must be a bare language code.
// A host is put behind https:// or sent as a Host header as it is: a
// scheme, path or space is refused at start, and host:port is a host.
func TestLoadRefusesAHostThatIsNotBare(t *testing.T) {
	for _, name := range []string{
		"DJANGO_PUBLIC_HOST", "ASSETS_HOST", "STAFF_CONNECT_HOST",
	} {
		for _, value := range []string{
			"https://connect.platform.test", "connect.platform.test/",
			"connect.platform.test ", "user@connect.platform.test",
		} {
			setRequired(t)
			t.Setenv(name, value)
			_, err := Load()
			require.Error(t, err, "%s=%q", name, value)
			assert.Contains(t, err.Error(), name)
		}
		setRequired(t)
		t.Setenv(name, "connect.localhost:8000")
		_, err := Load()
		require.NoError(t, err, "%s with a port", name)
	}
}

func TestLoadValidatesStorefrontDefaultLocale(t *testing.T) {
	for value, wantErr := range map[string]bool{
		"": true, "EL": true, "el-GR": true, "/el": true,
		"el": false, "en": false,
	} {
		t.Run(value, func(t *testing.T) {
			setRequired(t)
			t.Setenv("STOREFRONT_DEFAULT_LOCALE", value)

			cfg, err := Load()
			if wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "STOREFRONT_DEFAULT_LOCALE")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, value, cfg.StorefrontDefaultLocale)
		})
	}
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

	// Staging is the infra overlay's value: a public cluster that must
	// keep production's webhook rule while advertising itself as a
	// sandbox so platforms keep test traffic out of live order flow.
	t.Setenv("ENV", EnvStaging)
	cfg, err := Load()
	require.NoError(t, err)
	assert.False(t, cfg.AllowLocalWebhooks)
	assert.Equal(t, HandlerEnvSandbox, cfg.PaymentHandlerEnv)

	t.Setenv("ENV", "dev")
	_, err = Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ENV")
}

// LOG_LEVEL is parsed once, here: the logger takes the parsed level and
// has no default of its own to fall back on.
func TestLoadParsesLogLevel(t *testing.T) {
	for value, want := range map[string]slog.Level{
		"":      slog.LevelInfo,
		"debug": slog.LevelDebug,
		"WARN":  slog.LevelWarn,
		"error": slog.LevelError,
	} {
		t.Run(value, func(t *testing.T) {
			setRequired(t)
			t.Setenv("LOG_LEVEL", value)
			cfg, err := Load()
			require.NoError(t, err)
			assert.Equal(t, want, cfg.LogLevel)
		})
	}

	setRequired(t)
	t.Setenv("LOG_LEVEL", "verbose")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LOG_LEVEL")
}

// Both base URLs have paths appended to them: Django's by string
// concatenation, the chat provider's by URL resolution, which drops a
// query or fragment. Without a scheme and host every call fails while
// the pod stays Ready (readiness ignores Django on purpose).
func TestLoadValidatesBaseURLs(t *testing.T) {
	refused := []string{
		"backend-service/api/v1",
		"backend-service:8000/api/v1",
		"//backend-service/api/v1",
		"ftp://backend-service/api/v1",
		"http:///api/v1",
		"http://user:pw@backend-service/api/v1",
		"http://backend-service/api/v1?x=1",
		"http://backend-service/api/v1#frag",
		"http://backend service/api/v1",
	}
	for _, name := range []string{"DJANGO_BASE_URL", "CHAT_BASE_URL"} {
		for _, value := range refused {
			t.Run(name+"="+value, func(t *testing.T) {
				setRequired(t)
				t.Setenv(name, value)
				_, err := Load()
				require.Error(t, err)
				assert.Contains(t, err.Error(), name)
				assert.NotContains(t, err.Error(), "pw",
					"a credential in the URL is never echoed")
			})
		}
	}
	for value, want := range map[string]string{
		"http://backend-service/api/v1/": "http://backend-service/api/v1",
		"https://127.0.0.1:8000/api/v1":  "https://127.0.0.1:8000/api/v1",
	} {
		t.Run("DJANGO_BASE_URL="+value, func(t *testing.T) {
			setRequired(t)
			t.Setenv("DJANGO_BASE_URL", value)
			cfg, err := Load()
			require.NoError(t, err)
			assert.Equal(t, want, cfg.DjangoBaseURL)
		})
	}
	t.Run("CHAT_BASE_URL=http with port", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CHAT_BASE_URL", "http://127.0.0.1:11434/v1/")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, "http://127.0.0.1:11434/v1/", cfg.ChatBaseURL)
	})
}

// CHAT_EFFORT is sent as reasoning_effort on every model call: a value
// outside the API's set fails every chat turn.
func TestLoadValidatesChatEffort(t *testing.T) {
	for value, wantErr := range map[string]bool{
		"none": false, "minimal": false, "low": false, "medium": false,
		"high": false, "xhigh": false, "max": false,
		"Low": true, "lowest": true, "off": true, "auto": true,
	} {
		t.Run(value, func(t *testing.T) {
			setRequired(t)
			t.Setenv("CHAT_EFFORT", value)
			cfg, err := Load()
			if wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "CHAT_EFFORT")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, value, cfg.ChatEffort)
		})
	}
}

// An image template missing {path} names one image for every product;
// one missing {assets_host} ignores a tenant's own assets host; a
// misspelt placeholder is published verbatim. Feeds carry these URLs to
// Meta and TikTok, which never report a broken image back.
func TestLoadValidatesImageURLTemplates(t *testing.T) {
	refused := []string{
		"https://assets.example.com/media_stream-image/{path}",
		"https://{assets_host}/media_stream-image/image.jpg",
		"https://{assets_host}/{schema}/{paht}/{path}",
		"https://{assets_host}/{path}/{size}",
		"{assets_host}/media_stream-image/{path}",
		"ftp://{assets_host}/{path}",
		"https://user@{assets_host}/{path}",
	}
	for _, name := range []string{
		"MEDIA_IMAGE_URL_TEMPLATE", "FEED_IMAGE_URL_TEMPLATE",
	} {
		for _, value := range refused {
			t.Run(name+"="+value, func(t *testing.T) {
				setRequired(t)
				t.Setenv(name, value)
				_, err := Load()
				require.Error(t, err)
				assert.Contains(t, err.Error(), name)
			})
		}
		for _, value := range []string{
			"https://{assets_host}/{schema}/{path}",
			"http://{assets_host}:8080/x/{path}?w=800",
		} {
			t.Run(name+"="+value, func(t *testing.T) {
				setRequired(t)
				t.Setenv(name, value)
				_, err := Load()
				require.NoError(t, err)
			})
		}
	}
}

// The secret travels as a header both ways: a control character (a
// trailing newline from a file) makes Go refuse every request carrying
// it, and leading or trailing blanks are trimmed by the receiving
// server, so the comparison fails. Production and staging need a secret
// long enough not to be a placeholder; the error never echoes it.
func TestLoadValidatesInternalSecret(t *testing.T) {
	long := strings.Repeat("a", minInternalSecretLen)
	for name, tc := range map[string]struct {
		env, secret string
		wantErr     bool
	}{
		"trailing newline":     {EnvProduction, long + "\n", true},
		"carriage return":      {EnvProduction, long + "\r", true},
		"inner control char":   {EnvProduction, "ab\x1b" + long, true},
		"delete char":          {EnvProduction, long + "\x7f", true},
		"leading space":        {EnvProduction, " " + long, true},
		"trailing tab":         {EnvProduction, long + "\t", true},
		"short in production":  {EnvProduction, long[1:], true},
		"short in staging":     {EnvStaging, "change-me", true},
		"newline in dev":       {EnvDevelopment, "change-me\n", true},
		"minimum length":       {EnvProduction, long, false},
		"long in staging":      {EnvStaging, long + long, false},
		"short in development": {EnvDevelopment, "change-me", false},
		"short in test":        {EnvTest, "s3cret", false},
	} {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			t.Setenv("ENV", tc.env)
			t.Setenv("INTERNAL_EVENTS_SECRET", tc.secret)
			cfg, err := Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "INTERNAL_EVENTS_SECRET")
				assert.NotContains(t, err.Error(), strings.TrimSpace(tc.secret))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.secret, cfg.InternalSecret)
		})
	}
}

// A stale window shorter than the fresh one expires the cached feed
// before it is ever served stale: every request past it waits for a
// full generation.
func TestLoadFeedStaleTTLIsNotShorterThanFresh(t *testing.T) {
	for _, tc := range []struct {
		fresh, stale string
		wantErr      bool
	}{
		{"6h", "5h", true},
		{"", "1h", true},
		{"6h", "6h", false},
		{"1h", "", false},
	} {
		t.Run(tc.fresh+"/"+tc.stale, func(t *testing.T) {
			setRequired(t)
			t.Setenv("FEED_FRESH_TTL", tc.fresh)
			t.Setenv("FEED_STALE_TTL", tc.stale)
			_, err := Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "FEED_STALE_TTL")
				assert.Contains(t, err.Error(), "FEED_FRESH_TTL")
				return
			}
			require.NoError(t, err)
		})
	}
}
