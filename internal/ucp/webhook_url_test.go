package ucp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateWebhookURLAcceptsPublicHTTPS(t *testing.T) {
	for _, raw := range []string{
		"https://platform.example.com/hooks/orders",
		"https://hooks.acme.co.uk/x?y=1",
		"https://1.1.1.1/hook",
		"https://[2606:4700:4700::1111]/hook",
	} {
		assert.NoError(t, ValidateWebhookURL(raw, false), raw)
	}
}

func TestValidateWebhookURLAllowsEmpty(t *testing.T) {
	// No endpoint registered is a normal checkout, not an error.
	require.NoError(t, ValidateWebhookURL("", false))
}

// create_checkout is reachable ANONYMOUSLY, and the dispatcher POSTs to
// whatever is stored here on every order transition — so an unvalidated
// value turns the gateway into a blind request origin for in-cluster
// addresses, and a blackhole endpoint occupies a delivery worker for the
// full retry budget.
func TestValidateWebhookURLRejectsInternalTargets(t *testing.T) {
	cases := map[string]string{
		"plain http":      "http://platform.example.com/hook",
		"in-cluster host": "https://backend-service/api/v1/health",
		"localhost name":  "https://localhost/hook",
		"loopback ip":     "https://127.0.0.1/hook",
		"private ip":      "https://10.0.0.3/hook",
		"private ip 172":  "https://172.16.4.5/hook",
		"link-local":      "https://169.254.169.254/latest/meta-data",
		"unspecified":     "https://0.0.0.0/hook",
		"shared cgnat":    "https://100.64.1.1/hook",
		"documentation":   "https://203.0.113.10/hook",
		"ipv6 loopback":   "https://[::1]/hook",
		"ipv6 ula":        "https://[fd00::1]/hook",
		"v4-mapped":       "https://[::ffff:10.0.0.1]/hook",
		"cluster suffix":  "https://svc.cluster.local/hook",
		"internal suffix": "https://api.internal/hook",
		"mdns suffix":     "https://printer.local/hook",
		"missing host":    "https:///hook",
		"not a url":       "://nope",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateWebhookURL(raw, false)
			require.Error(t, err, raw)
			assert.ErrorIs(t, err, ErrWebhookURL)
		})
	}
}

// Development and the e2e suite point webhooks at httptest servers on
// 127.0.0.1; production must not.
func TestValidateWebhookURLAllowLocal(t *testing.T) {
	local := "http://127.0.0.1:54321/ucp/orders"
	require.Error(t, ValidateWebhookURL(local, false))
	require.NoError(t, ValidateWebhookURL(local, true))

	// Even relaxed, a nonsense scheme is still refused.
	require.Error(t, ValidateWebhookURL("ftp://127.0.0.1/x", true))
}

// A redirect would carry the signed request to a target no check saw.
func TestWebhookClientDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
	t.Cleanup(redirector.Close)

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, redirector.URL, nil)
	require.NoError(t, err)
	resp, err := webhookClient(true).Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	assert.False(t, reached.Load())
}

// The dial check sees the address actually connected to, so a public
// name resolving to a private address is refused too.
func TestWebhookClientRefusesSpecialUseAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, srv.URL, nil)
	require.NoError(t, err)
	_, err = webhookClient(false).Do(req)
	assert.ErrorIs(t, err, ErrWebhookURL)
}
