package ucp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpsig"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
)

// postTimeout bounds one post of an order webhook, from the dial to the
// end of the answer. The caller's context bounds it too.
const postTimeout = 15 * time.Second

// signedComponents are the RFC 9421 components a webhook covers, in the
// order UCP's REST request signing lists them: @query only when the
// platform's URL carries one. Webhook-Id and Webhook-Timestamp are
// covered too: UCP after 2026-08-25 requires it of an order webhook and
// has platforms reject a delivery whose signature omits either, and a
// verifier of 2026-08-25 rebuilds the base from the listed components,
// so covering two more costs it nothing.
func signedComponents(withQuery bool) []string {
	c := []string{"@method", "@authority", "@path"}
	if withQuery {
		c = append(c, "@query")
	}
	return append(c, "ucp-agent", "content-digest", "content-type",
		"webhook-id", "webhook-timestamp")
}

// Delivery is one order webhook: the event it reports, the order entity
// it carries and the platform it goes to.
type Delivery struct {
	// ID is the Webhook-Id platforms dedupe on: the event's own id, the
	// same on every attempt (Standard Webhooks; UCP's order capability).
	ID string
	// OccurredAt is when the event happened: the Webhook-Timestamp, the
	// same on every attempt (UCP: "Event occurrence timestamp").
	OccurredAt time.Time
	// Schema selects the signing key; Domain names the business profile
	// in UCP-Agent.
	Schema    string
	Domain    string
	TargetURL string
	// Body is the order entity, exactly as signed and sent.
	Body []byte
}

// Reply is a platform's answer to an order webhook.
type Reply struct {
	Status int
	// RetryAfter is the wait the platform asked for, zero when it asked
	// for none.
	RetryAfter time.Duration
}

// Delivered reports a 2xx.
func (r Reply) Delivered() bool {
	return r.Status >= 200 && r.Status < 300
}

// Retryable reports whether another attempt can change the answer: a
// 408, a 429 or a 5xx. Any other answer that is not a delivery refuses
// the event for good: a 4xx, or a 3xx, since a redirect is never
// followed.
func (r Reply) Retryable() bool {
	return r.Status == http.StatusRequestTimeout ||
		r.Status == http.StatusTooManyRequests ||
		r.Status >= http.StatusInternalServerError
}

// Webhooks signs and posts order webhooks, one attempt per call. The
// event, and when to try it again, are the caller's: Django's outbox
// keeps each event until a platform's answer settles it.
type Webhooks struct {
	keys *Keys
	hc   *http.Client
}

// NewWebhooks builds the sender. allowLocal is the ENV-driven
// AllowLocalWebhooks: only development and tests may deliver to
// loopback or private addresses.
func NewWebhooks(keys *Keys, allowLocal bool) *Webhooks {
	return &Webhooks{keys: keys, hc: platformClient(allowLocal, postTimeout)}
}

// Send signs dl with its store's key and posts it once. Headers follow
// Standard Webhooks (Webhook-Id, Webhook-Timestamp); the signature is
// RFC 9421 over the UCP REST components, verifiable against the JWK in
// the store's profile, which UCP-Agent names.
//
// An error is an attempt that got no answer: the key could not be read,
// or the post failed or ran out of time. Another attempt may deliver
// it, except one that wraps ErrPlatformURL: the target is not a URL the
// gateway may call.
func (w *Webhooks) Send(ctx context.Context, dl Delivery) (Reply, error) {
	if dl.TargetURL == "" || dl.ID == "" {
		return Reply{}, errors.New("ucp: delivery without a target or id")
	}
	// Nothing is signed before the pod's key-encryption key is known to
	// open the stored keys (AwaitKEK): a store's first key minted under
	// another one would be a key no other pod opens.
	if err := w.keys.Checked(); err != nil {
		return Reply{}, err
	}
	key, err := w.keys.ForSchema(ctx, dl.Schema)
	if err != nil {
		return Reply{}, fmt.Errorf("ucp: webhook signing key: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		dl.TargetURL, bytes.NewReader(dl.Body))
	if err != nil {
		return Reply{}, fmt.Errorf("%w: %w", ErrPlatformURL, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Webhook-Id", dl.ID)
	req.Header.Set("Webhook-Timestamp",
		strconv.FormatInt(dl.OccurredAt.Unix(), 10))
	req.Header.Set("UCP-Agent",
		`profile="`+storefront.UCPProfile(dl.Domain)+`"`)
	req.Header.Set("Content-Digest", httpsig.ContentDigest(dl.Body))
	// The signature's own creation time is this attempt's.
	if err := httpsig.Sign(req, "sig1",
		signedComponents(req.URL.RawQuery != ""),
		httpsig.Params{Created: time.Now().Unix()}, key); err != nil {
		return Reply{}, fmt.Errorf("ucp: webhook signing: %w", err)
	}

	resp, err := w.hc.Do(req)
	if err != nil {
		return Reply{}, fmt.Errorf("ucp: webhook post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the keep-alive connection is reusable for the next
	// delivery to the same platform.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return Reply{
		Status:     resp.StatusCode,
		RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}, nil
}

// retryAfter reads a Retry-After value (RFC 9110: delta-seconds or an
// HTTP date) as the wait from now; zero for none, one that cannot be
// read, or a date already past.
func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.ParseUint(value, 10, 31); err == nil {
		return time.Duration(secs) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}
