package ucp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateWebhookURLAcceptsPublicHTTPS(t *testing.T) {
	for _, raw := range []string{
		"https://platform.example.com/hooks/orders",
		"https://hooks.acme.co.uk/x?y=1",
		"https://203.0.113.10/hook",
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
