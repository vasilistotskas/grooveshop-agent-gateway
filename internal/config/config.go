package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every per-instance setting. Per-tenant values (branding,
// locale, feature flags, publishable keys) are served by Django's
// tenant/resolve endpoint at request time and must never appear here.
type Config struct {
	ListenAddr string
	Env        string
	LogLevel   string

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
	// URLs — which per docs/tenant-onboarding.md is the default: assets
	// hosts are not provisioned per tenant. A tenant that HAS opted in
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

// Load reads the environment and fails fast on missing required values so
// a misconfigured pod crashes at startup instead of serving errors.
func Load() (Config, error) {
	cfg := Config{
		ListenAddr:       envOr("LISTEN_ADDR", ":8080"),
		Env:              envOr("ENV", "production"),
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

	var err error
	if cfg.TenantCacheTTL, err = durationOr("TENANT_CACHE_TTL", 5*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.NegativeCacheTTL, err = durationOr("TENANT_NEGATIVE_CACHE_TTL", time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.UpstreamTimeout, err = durationOr("UPSTREAM_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitPerMin, err = intOr("RATE_LIMIT_PER_MIN", 120); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitBurst, err = intOr("RATE_LIMIT_BURST", 40); err != nil {
		return Config{}, err
	}
	if cfg.ChatMaxTokens, err = intOr("CHAT_MAX_TOKENS", 2048); err != nil {
		return Config{}, err
	}
	if cfg.ChatMaxTurns, err = intOr("CHAT_MAX_TURNS", 40); err != nil {
		return Config{}, err
	}
	if cfg.ChatMaxIterations, err = intOr("CHAT_MAX_ITERATIONS", 6); err != nil {
		return Config{}, err
	}
	if cfg.ChatRatePerMin, err = intOr("CHAT_RATE_LIMIT_PER_MIN", 20); err != nil {
		return Config{}, err
	}
	if cfg.ChatRateBurst, err = intOr("CHAT_RATE_LIMIT_BURST", 5); err != nil {
		return Config{}, err
	}
	if cfg.ChatMaxMessageLen, err = intOr("CHAT_MAX_MESSAGE_LEN", 2000); err != nil {
		return Config{}, err
	}
	if cfg.ConversationTTL, err = durationOr("CHAT_CONVERSATION_TTL", 24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.FeedFreshTTL, err = durationOr("FEED_FRESH_TTL", 6*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.FeedStaleTTL, err = durationOr("FEED_STALE_TTL", 24*time.Hour); err != nil {
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

func durationOr(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return d, nil
}

func intOr(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return n, nil
}
