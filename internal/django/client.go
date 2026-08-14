package django

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// Client is the typed Django API client shared by every surface. Tenancy
// travels on X-Forwarded-Host (django-tenants resolves the schema from it),
// so every tenant-scoped call passes the tenant's domain explicitly.
type Client struct {
	hc             *http.Client
	baseURL        string
	publicHost     string
	internalSecret string
	log            *slog.Logger
	metrics        *obs.Metrics
}

func New(
	baseURL, publicHost, internalSecret string,
	timeout time.Duration,
	log *slog.Logger,
	metrics *obs.Metrics,
) *Client {
	transport := &http.Transport{
		MaxIdleConnsPerHost: 32,
		MaxConnsPerHost:     64,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		hc:             &http.Client{Transport: transport, Timeout: timeout},
		baseURL:        baseURL,
		publicHost:     publicHost,
		internalSecret: internalSecret,
		log:            log,
		metrics:        metrics,
	}
}

// request describes one upstream call. Path is the templated path used for
// metrics labels; query/body/headers are optional.
type request struct {
	method   string
	path     string // templated, e.g. /product/{id}
	rawPath  string // actual, e.g. /product/42; defaults to path
	host     string // X-Forwarded-Host value
	language string
	query    url.Values
	body     any
	headers  map[string]string
	out      any
}

// Ping reports Django reachability for /readyz's informational field.
func (c *Client) Ping(ctx context.Context) error {
	return c.get(ctx, request{
		path: "/health/live",
		host: c.publicHost,
	})
}

// ResolveTenant maps a storefront domain to its tenant configuration. It is
// the only host-unscoped call: the platform API host satisfies Django's
// ALLOWED_HOSTS check while the domain being resolved rides the query.
func (c *Client) ResolveTenant(ctx context.Context, domain string) (*TenantConfig, error) {
	var out TenantConfig
	err := c.get(ctx, request{
		path:  "/tenant/resolve",
		host:  c.publicHost,
		query: url.Values{"domain": {domain}},
		out:   &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// get performs a GET with bounded retries. Only GETs are retried —
// mutations are not idempotent at the transport level; checkout idempotency
// lives in the checkout package.
func (c *Client) get(ctx context.Context, req request) error {
	req.method = http.MethodGet

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			delay := time.Duration(100*(1<<(attempt-1)))*time.Millisecond +
				time.Duration(rand.IntN(100))*time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		err := c.do(ctx, req)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable(err) {
			return err
		}
	}
	return lastErr
}

// send performs a non-GET call exactly once.
func (c *Client) send(ctx context.Context, method string, req request) error {
	req.method = method
	return c.do(ctx, req)
}

func (c *Client) do(ctx context.Context, r request) error {
	path := r.rawPath
	if path == "" {
		path = r.path
	}
	u := c.baseURL + path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}

	var body io.Reader
	if r.body != nil {
		raw, err := json.Marshal(r.body)
		if err != nil {
			return fmt.Errorf("django: encode body: %w", err)
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return fmt.Errorf("django: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", r.host)
	if r.language != "" {
		req.Header.Set("X-Language", r.language)
	}
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := c.hc.Do(req)
	elapsed := time.Since(start)

	code := "error"
	if resp != nil {
		code = strconv.Itoa(resp.StatusCode)
	}
	if c.metrics != nil {
		c.metrics.UpstreamRequests.WithLabelValues(r.path, r.method, code).Inc()
		c.metrics.UpstreamDuration.WithLabelValues(r.path).Observe(elapsed.Seconds())
	}
	if err != nil {
		c.log.WarnContext(ctx, "django request failed",
			slog.String("upstream", r.path),
			slog.String("error", err.Error()),
			slog.Int64("upstream_ms", elapsed.Milliseconds()),
		)
		return fmt.Errorf("%w: %s", ErrUpstreamDown, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return c.apiError(resp)
	}
	if r.out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(r.out); err != nil {
		return fmt.Errorf("django: decode %s: %w", r.path, err)
	}
	return nil
}

func (c *Client) apiError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	detail := ""
	var parsed struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		detail = parsed.Detail
	}
	if detail == "" {
		detail = http.StatusText(resp.StatusCode)
	}
	return &APIError{Status: resp.StatusCode, Detail: detail}
}

func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, ErrUpstreamDown)
}
