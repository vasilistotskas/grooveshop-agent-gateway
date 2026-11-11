package django

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/text"
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
		hc: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// A redirect is an error (redirected), as in
			// internal/ucp/fetch.go: no Django path the client calls
			// redirects, and following one would replay the call.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		baseURL:        baseURL,
		publicHost:     publicHost,
		internalSecret: internalSecret,
		log:            log,
		metrics:        metrics,
	}
}

// Timeout is the longest one call waits for Django.
func (c *Client) Timeout() time.Duration {
	return c.hc.Timeout
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
// One attempt: the probe repeats on its own period.
func (c *Client) Ping(ctx context.Context) error {
	return c.send(ctx, http.MethodGet, request{
		path: "/health/live",
		host: c.publicHost,
	})
}

// ResolveTenant maps a storefront domain to its tenant configuration. It is
// the only host-unscoped call: the platform API host satisfies Django's
// ALLOWED_HOSTS check while the domain being resolved rides the query.
// The internal secret authenticates the gateway so Django includes the
// tenant's secret fields (chatApiKey) that public callers never see.
func (c *Client) ResolveTenant(ctx context.Context, domain string) (*TenantConfig, error) {
	var out TenantConfig
	err := c.get(ctx, request{
		path:    "/tenant/resolve",
		host:    c.publicHost,
		query:   url.Values{"domain": {domain}},
		headers: map[string]string{"X-Internal-Token": c.internalSecret},
		out:     &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// get performs a GET with bounded retries (retryable). Only GETs are
// retried — mutations are not idempotent at the transport level; checkout
// idempotency lives in the checkout package. A caller whose context has
// ended is never retried.
func (c *Client) get(ctx context.Context, req request) error {
	req.method = http.MethodGet

	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			delay := time.Duration(100*(1<<(attempt-1)))*time.Millisecond +
				time.Duration(rand.IntN(100))*time.Millisecond
			select {
			case <-ctx.Done():
				return callerGone(ctx, req)
			case <-time.After(delay):
			}
		}

		err := c.do(ctx, req)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil || !retryable(err) {
			return err
		}
	}
	return lastErr
}

// send performs a call exactly once: every mutation, and Ping.
func (c *Client) send(ctx context.Context, method string, req request) error {
	req.method = method
	if key := idempotencyKeyFrom(ctx); key != "" {
		req.headers = maps.Clone(req.headers)
		if req.headers == nil {
			req.headers = map[string]string{}
		}
		req.headers["Idempotency-Key"] = key
	}
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
	setCaller(ctx, req.Header)
	// Django logs a request under this id (core/middleware/
	// correlation_id.py), so one id finds both sides of a call.
	if id := obs.RequestID(ctx); id != "" {
		req.Header.Set("X-Correlation-ID", id)
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := c.hc.Do(req)
	elapsed := time.Since(start)

	code := "error"
	switch {
	case resp != nil:
		code = strconv.Itoa(resp.StatusCode)
	case ctx.Err() != nil:
		code = "canceled"
	}
	if c.metrics != nil {
		c.metrics.UpstreamRequests.WithLabelValues(r.path, r.method, code).Inc()
		c.metrics.UpstreamDuration.WithLabelValues(r.path).Observe(elapsed.Seconds())
	}
	if err != nil && ctx.Err() != nil {
		return callerGone(ctx, r)
	}
	if err != nil {
		err = withoutQuery(err)
		c.log.WarnContext(ctx, "django request failed",
			slog.String("upstream", r.path),
			slog.String("error", err.Error()),
			slog.Int64("upstream_ms", elapsed.Milliseconds()),
		)
		return fmt.Errorf("%w: %w", ErrUpstreamDown, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return c.apiError(resp)
	}
	if resp.StatusCode >= 300 {
		return c.redirected(ctx, r, resp)
	}
	if r.out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(r.out); err != nil {
		if ctx.Err() != nil {
			return callerGone(ctx, r)
		}
		return fmt.Errorf("django: decode %s: %w", r.path, err)
	}
	return nil
}

// callerGone is the error of a call whose caller's context ended: it
// wraps ctx.Err() and is never ErrUpstreamDown, since Django did not
// fail. It is neither logged nor retried.
func callerGone(ctx context.Context, r request) error {
	return fmt.Errorf("django: %s %s: %w", r.method, r.path, ctx.Err())
}

// redirected turns a 3xx into ErrUpstreamDown, never retried: Django
// did not answer the call it was sent, and a mutation it redirected
// may have run. Location is logged without its query.
func (c *Client) redirected(
	ctx context.Context, r request, resp *http.Response,
) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	location := resp.Header.Get("Location")
	if u, err := url.Parse(location); err == nil {
		u.RawQuery, u.ForceQuery, u.Fragment = "", false, ""
		location = u.String()
	} else {
		location = "(unparsable)"
	}
	c.log.WarnContext(ctx, "django redirected a call",
		slog.String("upstream", r.path),
		slog.Int("status", resp.StatusCode),
		slog.String("location", location),
	)
	return fmt.Errorf("%w: %s %s redirected with status %d",
		ErrUpstreamDown, r.method, r.path, resp.StatusCode)
}

// maxErrorBody bounds how much of an upstream error body is retained; the
// largest structured failure is a stock shortfall listing cart lines.
const maxErrorBody = 64 << 10

func (c *Client) apiError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	apiErr := &APIError{Status: resp.StatusCode, Body: body}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil &&
		s > 0 {
		apiErr.RetryAfter = time.Duration(s) * time.Second
	}
	// Each member is decoded on its own: `code` is a coupon code's field
	// errors (a list) when the request had a `code` field, and one
	// mistyped member must not cost the others.
	var parsed map[string]json.RawMessage
	if json.Unmarshal(body, &parsed) != nil {
		return apiErr
	}
	_ = json.Unmarshal(parsed["detail"], &apiErr.Detail)
	_ = json.Unmarshal(parsed["code"], &apiErr.Code)
	_ = json.Unmarshal(parsed["reason"], &apiErr.Reason)
	if apiErr.Detail == "" {
		var fields any
		if json.Unmarshal(parsed["fields"], &fields) == nil {
			apiErr.Detail = text.Ellipsize(
				strings.Join(fieldMessages("", fields), "; "), 512)
		}
	}
	return apiErr
}

// fieldMessages flattens a validation error's `fields` ({field:
// [{message, code}]}, nested where the serializer nests) into
// "field: message" lines, the nested path dotted.
func fieldMessages(path string, v any) []string {
	var out []string
	switch v := v.(type) {
	case map[string]any:
		if msg, ok := v["message"].(string); ok {
			if path == "" || path == "nonFieldErrors" || path == "detail" {
				return []string{msg}
			}
			return []string{path + ": " + msg}
		}
		keys := slices.Sorted(maps.Keys(v))
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			out = append(out, fieldMessages(p, v[k])...)
		}
	case []any:
		for _, item := range v {
			out = append(out, fieldMessages(path, item)...)
		}
	}
	return out
}

// retryable: a 5xx, or a connection that failed before Django answered
// (refused, reset, closed). Not a call cut by the client's timeout: a
// Django that hangs would get the same call twice more while it hangs,
// and the caller would wait three timeouts instead of one. Not a
// redirect either.
func retryable(err error) bool {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.Status >= 500
	}
	netErr, ok := errors.AsType[net.Error](err)
	return ok && !netErr.Timeout()
}

// withoutQuery drops the query string from a transport error's URL. A
// *url.Error prints the whole URL, and a query can carry what a person
// typed: a shopper's search, or a customer's email a store's staff looked
// up. Neither belongs in a log line or an error a caller may log.
func withoutQuery(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	u, parseErr := url.Parse(ue.URL)
	if parseErr != nil {
		return &url.Error{Op: ue.Op, URL: "(unparsable URL)", Err: ue.Err}
	}
	u.RawQuery = ""
	u.ForceQuery = false
	return &url.Error{Op: ue.Op, URL: u.String(), Err: ue.Err}
}
