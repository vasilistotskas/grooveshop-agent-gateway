package ucp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testWebhooks is a sender whose keys hold the store's key in memory, as
// a pod does after its first use, and passed the boot check.
func testWebhooks(t *testing.T, allowLocal bool) (*Webhooks, *SigningKey) {
	t.Helper()
	keys := sealingKeys(t, testKEK(t))
	key := testKey(t)
	keys.remember("demostore", key)
	keys.checked.Store(true)
	return NewWebhooks(keys, allowLocal), key
}

// testDelivery is a delivery to target, of an event that happened an
// hour ago.
func testDelivery(target string) Delivery {
	return Delivery{
		ID:         "0193a6f0-7c1e-7b3a-9c4d-5e6f7a8b9c0d",
		OccurredAt: time.Date(2026, 11, 18, 2, 0, 0, 123456000, time.UTC),
		Schema:     "demostore",
		Domain:     "shop.example",
		TargetURL:  target,
		Body:       []byte(`{"id":"order"}`),
	}
}

// A webhook carries the event's own id and time, never the attempt's:
// Standard Webhooks and UCP require both unchanged across retries, so a
// platform dedupes every attempt of one event. Its signature covers
// both, and its body is the entity as given.
func TestSendCarriesTheEventsIDAndTime(t *testing.T) {
	var got *http.Request
	var body []byte
	platform := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			got = r.Clone(r.Context())
			body, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
		}))
	t.Cleanup(platform.Close)
	hooks, key := testWebhooks(t, true)
	dl := testDelivery(platform.URL + "/orders")

	for range 2 {
		reply, err := hooks.Send(t.Context(), dl)
		require.NoError(t, err)
		assert.True(t, reply.Delivered())
		assert.Equal(t, dl.ID, got.Header.Get("Webhook-Id"))
		assert.Equal(t, strconv.FormatInt(dl.OccurredAt.Unix(), 10),
			got.Header.Get("Webhook-Timestamp"))
	}
	assert.Equal(t, dl.Body, body)
	assert.Equal(t, `profile="https://shop.example/.well-known/ucp"`,
		got.Header.Get("UCP-Agent"))
	input := got.Header.Get("Signature-Input")
	assert.Contains(t, input, `"webhook-id" "webhook-timestamp"`)
	assert.Contains(t, input, `keyid="`+key.KID+`"`)
}

// The answer is the platform's: a 2xx is delivered; a 408, a 429 and a
// 5xx are worth another attempt, with the wait the platform asked for;
// any other answer refuses the event for good, a redirect included,
// since none is followed.
func TestSendReportsThePlatformsAnswer(t *testing.T) {
	for _, tc := range []struct {
		status     int
		retryAfter string
		delivered  bool
		retryable  bool
		wait       time.Duration
	}{
		{status: http.StatusOK, delivered: true},
		{status: http.StatusNoContent, delivered: true},
		{status: http.StatusTooManyRequests, retryAfter: "120",
			retryable: true, wait: 2 * time.Minute},
		{status: http.StatusServiceUnavailable, retryable: true},
		{status: http.StatusRequestTimeout, retryable: true},
		{status: http.StatusBadGateway, retryAfter: "soon", retryable: true},
		{status: http.StatusBadRequest},
		{status: http.StatusUnauthorized},
		{status: http.StatusGone},
		{status: http.StatusFound},
	} {
		platform := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				if tc.status == http.StatusFound {
					w.Header().Set("Location", "http://127.0.0.1:1/")
				}
				w.WriteHeader(tc.status)
			}))
		hooks, _ := testWebhooks(t, true)
		reply, err := hooks.Send(t.Context(), testDelivery(platform.URL))
		platform.Close()
		require.NoError(t, err, tc.status)
		assert.Equal(t, tc.status, reply.Status)
		assert.Equal(t, tc.delivered, reply.Delivered(), tc.status)
		assert.Equal(t, tc.retryable, reply.Retryable(), tc.status)
		assert.Equal(t, tc.wait, reply.RetryAfter, tc.status)
	}
}

// A platform that does not answer holds the call no longer than the
// caller's context: the gateway owes Django its answer in time.
func TestSendIsBoundedByItsContext(t *testing.T) {
	release := make(chan struct{})
	platform := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(platform.Close)
	t.Cleanup(func() { close(release) })
	hooks, _ := testWebhooks(t, true)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := hooks.Send(ctx, testDelivery(platform.URL+"/hook?token=s3cret"))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrPlatformURL, "an outage, worth a retry")
	assert.NotContains(t, err.Error(), "s3cret",
		"a token in the platform's URL is not logged")
	assert.Less(t, time.Since(start), 5*time.Second)
}

// A target that is not a public address is refused at the dial, as
// ErrPlatformURL: no retry changes it.
func TestSendRefusesANonPublicTarget(t *testing.T) {
	platform := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	t.Cleanup(platform.Close)
	hooks, _ := testWebhooks(t, false)
	_, err := hooks.Send(t.Context(), testDelivery(platform.URL))
	assert.ErrorIs(t, err, ErrPlatformURL)
}

// Nothing is signed before the boot check of the key-encryption key
// passed, and nothing is sent.
func TestSendSignsNothingBeforeTheKeysAreChecked(t *testing.T) {
	called := false
	platform := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		}))
	t.Cleanup(platform.Close)
	hooks, _ := testWebhooks(t, true)
	hooks.keys.checked.Store(false)
	_, err := hooks.Send(t.Context(), testDelivery(platform.URL))
	require.ErrorIs(t, err, errKEKUnchecked)
	assert.False(t, called)
}

// Retry-After is delta-seconds or an HTTP date (RFC 9110); a value that
// is neither, or a date already past, asks for no wait.
func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Duration{
		"":                              0,
		"90":                            90 * time.Second,
		" 30 ":                          30 * time.Second,
		"-5":                            0,
		"1.5":                           0,
		"tomorrow":                      0,
		"Wed, 18 Nov 2026 03:05:00 GMT": 5 * time.Minute,
		"Wed, 18 Nov 2026 02:55:00 GMT": 0,
	} {
		assert.Equal(t, want, retryAfter(value, now), "%q", value)
	}
}
