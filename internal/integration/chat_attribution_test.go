//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attributionDjango records the attribution each cart add sent; nil for
// an add that sent none.
func attributionDjango(t *testing.T) (http.Handler, func() []any) {
	t.Helper()
	base := fakeDjangoMux(t)
	var (
		mu   sync.Mutex
		seen []any
	)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/cart/item" {
			var body map[string]any
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			mu.Lock()
			seen = append(seen, body["attribution"])
			mu.Unlock()
		}
		base.ServeHTTP(w, r)
	})
	return h, func() []any {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

// The storefront relays the tab's first touch with the turn, and the
// assistant's add sends it on, so a cart filled through the chat keeps
// the visit's source. agentProtocol never passes: Django believes it
// from the gateway, and a shopper must not book their cart as an AI
// agent's. A turn that relays none sends none.
func TestChatAddSendsTheShopperAttribution(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
		textTurnSSE("Το πρόσθεσα."),
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
		textTurnSSE("Το πρόσθεσα."),
	}}
	djangoH, adds := attributionDjango(t)
	gw, _ := newChatGateway(t, fake, djangoH, quietLogger())

	postChat(t, gw.URL, map[string]any{
		"message": "βάλε το", "cartId": chatCartID,
		"attribution": map[string]any{
			"utmSource": "instagram", "utmMedium": "paid",
			"utmCampaign": "autumn", "clickIds": []string{"fbclid"},
			"referrer":      "https://l.instagram.com",
			"landingPath":   "/products/1",
			"agentProtocol": "ucp",
		},
	})
	postChat(t, gw.URL, map[string]any{
		"message": "κι άλλο", "cartId": chatCartID})

	got := adds()
	require.Len(t, got, 2)
	assert.Equal(t, map[string]any{
		"utmSource": "instagram", "utmMedium": "paid",
		"utmCampaign": "autumn", "clickIds": []any{"fbclid"},
		"referrer":    "https://l.instagram.com",
		"landingPath": "/products/1",
	}, got[0])
	assert.Nil(t, got[1])
}

// An attribution of the wrong shape is a malformed request, refused like
// any other before the turn spends anything.
func TestChatRefusesAMalformedAttribution(t *testing.T) {
	gw := startChatGateway(t, &fakeChatAPI{})

	status, _ := postChatFrom(t, gw.URL, "192.0.2.9", map[string]any{
		"message": "γεια", "attribution": map[string]any{"clickIds": "gclid"}})

	assert.Equal(t, http.StatusBadRequest, status)
}
