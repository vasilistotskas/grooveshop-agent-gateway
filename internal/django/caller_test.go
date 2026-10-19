package django

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callerHeaders makes one Django call while serving inbound and returns
// the headers Django received.
func callerHeaders(t *testing.T, inbound http.Header) http.Header {
	t.Helper()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "cart_with_items.json"))
		}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)

	h := RelayCaller(http.HandlerFunc(
		func(_ http.ResponseWriter, r *http.Request) {
			_, err := c.GetCart(r.Context(), "shop.example.test", "el",
				fixtureCartID)
			require.NoError(t, err)
		}))
	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	req.Header = inbound
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.NotNil(t, got)
	return got
}

// Django hears the visitor the storefront's SSR proxy would name: the
// address Cloudflare connected, as X-Real-IP, beside the edge proof and
// the X-Forwarded-For chain, all as they reached the gateway.
func TestRelayCallerNamesTheVisitorToDjango(t *testing.T) {
	got := callerHeaders(t, http.Header{
		"Cf-Connecting-Ip": {"203.0.113.7"},
		"X-Forwarded-For":  {"203.0.113.7, 172.70.1.1"},
		"X-Origin-Verify":  {"edge-proof"},
		"User-Agent":       {"Mozilla/5.0 shopper"},
		// Traefik's own peer, never the visitor.
		"X-Real-Ip": {"172.70.1.1"},
	})

	assert.Equal(t, "203.0.113.7", got.Get("X-Real-IP"))
	assert.Equal(t, "203.0.113.7, 172.70.1.1", got.Get("X-Forwarded-For"))
	assert.Equal(t, "edge-proof", got.Get("X-Origin-Verify"))
	assert.Equal(t, "Mozilla/5.0 shopper", got.Get("User-Agent"))
}

// The gateway relays and never fills in: a caller that brought no
// Cloudflare address gets none from the gateway, whatever X-Real-Ip it
// sent, and no proof it did not bring.
func TestRelayCallerVouchesForNothing(t *testing.T) {
	got := callerHeaders(t, http.Header{
		"X-Real-Ip":       {"198.51.100.66"},
		"X-Forwarded-For": {"198.51.100.66"},
	})

	assert.Empty(t, got.Get("X-Real-IP"))
	assert.Empty(t, got.Get("X-Origin-Verify"))
	assert.Equal(t, "198.51.100.66", got.Get("X-Forwarded-For"))
}

// A call made outside any request (a webhook delivery, a feed rebuild)
// names no caller.
func TestACallWithoutARequestNamesNoCaller(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "cart_with_items.json"))
		}))
	t.Cleanup(srv.Close)

	_, err := newTestClient(t, srv.URL).GetCart(context.Background(),
		"shop.example.test", "el", fixtureCartID)

	require.NoError(t, err)
	assert.Empty(t, got.Get("X-Real-IP"))
	assert.Empty(t, got.Get("X-Forwarded-For"))
	assert.Empty(t, got.Get("X-Origin-Verify"))
}
