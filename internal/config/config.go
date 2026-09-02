package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Deployment environments. ENV is validated against this set so a typo
// cannot silently select the strict production behaviour (or, worse, a
// relaxed one).
const (
	EnvProduction  = "production"
	EnvDevelopment = "development"
	EnvTest        = "test"
)

// Values of Config.PaymentHandlerEnv: the two environments the UCP
// payment handler's config schema admits.
const (
	HandlerEnvProduction = "production"
	HandlerEnvSandbox    = "sandbox"
)

// Config holds every per-instance setting. Per-tenant values (branding,
// locale, feature flags, publishable keys) are served by Django's
// tenant/resolve endpoint at request time and must never appear here.
type Config struct {
	ListenAddr string
	Env        string
	LogLevel   string

	// AllowLocalWebhooks relaxes UCP webhook-endpoint validation to any
	// http(s) host. Derived from Env here — and only here — so handlers
	// consume a value, not an environment check: development and the
	// e2e suite register httptest servers on 127.0.0.1, while production
	// must never originate requests to anything but a public https
	// endpoint.
	AllowLocalWebhooks bool
	// PaymentHandlerEnv is what the UCP payment handler advertises as its
	// `environment`. Only the production deployment reads as production:
	// every other Env is a sandbox, so a platform keeps its test traffic
	// out of live order flow. Derived from Env here, like
	// AllowLocalWebhooks.
	PaymentHandlerEnv string

	// DjangoBaseURL is the in-cluster API root, e.g.
	// http://backend-service/api/v1 (no trailing slash).
	DjangoBaseURL string
	// DjangoPublicHost is sent as X-Forwarded-Host on calls that are not
	// scoped to a tenant (tenant/resolve and the health ping) so
	// Django's ALLOWED_HOSTS + TenantMainMiddleware checks pass.
	// Point it at the PLATFORM control-plane host (the public-schema
	// TenantDomain created by ``manage.py bootstrap_platform``) — both
	// endpoints live on Django's public urlconf. Tenant-scoped calls
	// send the tenant's own domain instead.
	DjangoPublicHost string
	// InternalSecret authenticates Django's order-event pushes to
	// /internal/* routes.
	InternalSecret string

	RedisURL string

	// AssetsHost is the PLATFORM media origin (e.g. assets.example.com),
	// used for every tenant that has not opted into white-label asset
	// URLs — which, per the infra repo's docs/tenant-onboarding.md, is
	// the default: assets hosts are not provisioned per tenant. A tenant that HAS opted in
	// carries its own host in TenantConfig.AssetsDomain, which wins.
	// Deliberately has no default: guessing a hostname here is what
	// produced unreachable image URLs, and an empty value now yields an
	// empty image URL instead.
	AssetsHost string

	// MediaURLTemplate builds public product-image URLs. Placeholders:
	// {assets_host} (resolved via media.Host), {schema} (tenant schema),
	// {path} (relative mainImagePath). The full media-stream segment list
	// lives in the template so the MT cutover is a config flip.
	MediaURLTemplate string

	TenantCacheTTL   time.Duration
	NegativeCacheTTL time.Duration
	UpstreamTimeout  time.Duration

	RateLimitPerMin int
	RateLimitBurst  int

	// FeedImageURLTemplate builds feed image URLs — Meta/TikTok require
	// >=500x500 JPEG/PNG (never WebP), hence a separate template from
	// MediaURLTemplate. Same placeholders.
	FeedImageURLTemplate string
	FeedFreshTTL         time.Duration
	FeedStaleTTL         time.Duration

	// Chat (first-party shopping assistant), spoken over the
	// OpenAI-compatible chat-completions protocol. ChatBaseURL selects
	// the provider (default: Gemini's compatibility endpoint — its free
	// tier has the best Greek of the zero-cost options); Groq, Mistral,
	// OpenRouter, Z.ai or Anthropic's compat layer are config swaps.
	// The credential is per-tenant: it arrives on tenant/resolve
	// (chatApiKey), never from the environment.
	ChatBaseURL       string
	ChatModel         string
	ChatEffort        string
	ChatMaxTokens     int
	ChatMaxTurns      int
	ChatMaxIterations int
	ChatRatePerMin    int
	ChatRateBurst     int
	ConversationTTL   time.Duration
	ChatMaxMessageLen int
}

// Load reads the environment and fails fast on missing or malformed
// values so a misconfigured pod crashes at startup instead of serving
// errors.
func Load() (Config, error) {
	cfg := Config{
		ListenAddr:       envOr("LISTEN_ADDR", ":8080"),
		Env:              envOr("ENV", EnvProduction),
		LogLevel:         envOr("LOG_LEVEL", "info"),
		DjangoBaseURL:    strings.TrimRight(os.Getenv("DJANGO_BASE_URL"), "/"),
		DjangoPublicHost: os.Getenv("DJANGO_PUBLIC_HOST"),
		InternalSecret:   os.Getenv("INTERNAL_EVENTS_SECRET"),
		RedisURL:         os.Getenv("REDIS_URL"),
		AssetsHost:       os.Getenv("ASSETS_HOST"),
		MediaURLTemplate: envOr("MEDIA_IMAGE_URL_TEMPLATE",
			"https://{assets_host}/media_stream-image/{path}"+
				"/800/800/contain/entropy/transparent/5/80.webp"),
		ChatBaseURL: envOr("CHAT_BASE_URL",
			"https://generativelanguage.googleapis.com/v1beta/openai/"),
		ChatModel:  envOr("CHAT_MODEL", "gemini-3.5-flash-lite"),
		ChatEffort: envOr("CHAT_EFFORT", "low"),
		FeedImageURLTemplate: envOr("FEED_IMAGE_URL_TEMPLATE",
			"https://{assets_host}/media_stream-image/{path}"+
				"/1000/1000/contain/center/FFFFFF/5/85.jpeg"),
	}

	switch cfg.Env {
	case EnvProduction:
		cfg.PaymentHandlerEnv = HandlerEnvProduction
	case EnvDevelopment, EnvTest:
		cfg.AllowLocalWebhooks = true
		cfg.PaymentHandlerEnv = HandlerEnvSandbox
	default:
		return Config{}, fmt.Errorf(
			"config: ENV must be %s, %s or %s (got %q)",
			EnvProduction, EnvDevelopment, EnvTest, cfg.Env)
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return Config{}, fmt.Errorf("config: LOG_LEVEL: %w", err)
	}

	var err error
	if cfg.TenantCacheTTL, err = parseOr("TENANT_CACHE_TTL", 5*time.Minute, time.ParseDuration); err != nil {
		return Config{}, err
	}
	if cfg.NegativeCacheTTL, err = parseOr("TENANT_NEGATIVE_CACHE_TTL", time.Minute, time.ParseDuration); err != nil {
		return Config{}, err
	}
	if cfg.UpstreamTimeout, err = parseOr("UPSTREAM_TIMEOUT", 10*time.Second, time.ParseDuration); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitPerMin, err = parseOr("RATE_LIMIT_PER_MIN", 120, strconv.Atoi); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitBurst, err = parseOr("RATE_LIMIT_BURST", 40, strconv.Atoi); err != nil {
		return Config{}, err
	}
	if cfg.ChatMaxTokens, err = parseOr("CHAT_MAX_TOKENS", 2048, strconv.Atoi); err != nil {
		return Config{}, err
	}
	if cfg.ChatMaxTurns, err = parseOr("CHAT_MAX_TURNS", 40, strconv.Atoi); err != nil {
		return Config{}, err
	}
	if cfg.ChatMaxIterations, err = parseOr("CHAT_MAX_ITERATIONS", 6, strconv.Atoi); err != nil {
		return Config{}, err
	}
	if cfg.ChatRatePerMin, err = parseOr("CHAT_RATE_LIMIT_PER_MIN", 20, strconv.Atoi); err != nil {
		return Config{}, err
	}
	if cfg.ChatRateBurst, err = parseOr("CHAT_RATE_LIMIT_BURST", 5, strconv.Atoi); err != nil {
		return Config{}, err
	}
	if cfg.ChatMaxMessageLen, err = parseOr("CHAT_MAX_MESSAGE_LEN", 2000, strconv.Atoi); err != nil {
		return Config{}, err
	}
	if cfg.ConversationTTL, err = parseOr("CHAT_CONVERSATION_TTL", 24*time.Hour, time.ParseDuration); err != nil {
		return Config{}, err
	}
	if cfg.FeedFreshTTL, err = parseOr("FEED_FRESH_TTL", 6*time.Hour, time.ParseDuration); err != nil {
		return Config{}, err
	}
	if cfg.FeedStaleTTL, err = parseOr("FEED_STALE_TTL", 24*time.Hour, time.ParseDuration); err != nil {
		return Config{}, err
	}

	var missing []string
	if cfg.DjangoBaseURL == "" {
		missing = append(missing, "DJANGO_BASE_URL")
	}
	if cfg.DjangoPublicHost == "" {
		missing = append(missing, "DJANGO_PUBLIC_HOST")
	}
	if cfg.RedisURL == "" {
		missing = append(missing, "REDIS_URL")
	}
	if cfg.InternalSecret == "" {
		missing = append(missing, "INTERNAL_EVENTS_SECRET")
	}
	// Required rather than defaulted: every deployment serves product
	// images, and the previous behaviour — deriving assets.<tenant
	// domain> — produced URLs that resolve nowhere for any tenant on the
	// documented onboarding path. Feeds and agent responses carry those
	// URLs to Meta, TikTok and shopping agents, none of which report a
	// broken image back. Crash at startup instead.
	if cfg.AssetsHost == "" {
		missing = append(missing, "ASSETS_HOST")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf(
			"config: missing required env vars: %s",
			strings.Join(missing, ", "),
		)
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseOr reads key through parse, returning def when the variable is
// unset and a key-labelled error when it is set but malformed.
func parseOr[T any](key string, def T, parse func(string) (T, error)) (T, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	out, err := parse(v)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("config: %s: %w", key, err)
	}
	return out, nil
}
