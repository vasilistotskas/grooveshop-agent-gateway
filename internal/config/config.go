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
	// scoped to a tenant (only tenant/resolve today) so Django's
	// ALLOWED_HOSTS validation passes. Tenant-scoped calls send the
	// tenant's own domain instead.
	DjangoPublicHost string
	// InternalSecret authenticates Django's order-event pushes to
	// /internal/* routes.
	InternalSecret string

	RedisURL string

	// MediaURLTemplate builds public product-image URLs. Placeholders:
	// {domain} (tenant storefront domain), {schema} (tenant schema),
	// {path} (relative mainImagePath). The full media-stream segment list
	// lives in the template so the MT cutover is a config flip.
	MediaURLTemplate string

	TenantCacheTTL   time.Duration
	NegativeCacheTTL time.Duration
	UpstreamTimeout  time.Duration

	RateLimitPerMin int
	RateLimitBurst  int
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
		MediaURLTemplate: envOr("MEDIA_IMAGE_URL_TEMPLATE",
			"https://assets.{domain}/media_stream-image/{path}"+
				"/800/800/contain/entropy/transparent/5/80.webp"),
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
