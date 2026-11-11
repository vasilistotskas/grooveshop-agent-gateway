package mcpsrv

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Without a linked account, or with a token missing the tool's scope, the
// agent gets a ToolError whose code says which, and what to do.
func TestAccountToolsSayWhatTheLinkLacks(t *testing.T) {
	store := tenant.NewContext(context.Background(), cartTenant())

	t.Run("no linked account", func(t *testing.T) {
		h := fixtureHandlers(t, "agent_orders.json", func(*http.Request) {
			t.Error("an unlinked call must not reach the store")
		})
		_, _, err := h.myOrders(store, nil, struct{}{})

		var got *ToolError
		require.ErrorAs(t, err, &got)
		assert.Equal(t, "unauthorized", got.Code)
		assert.Contains(t, got.Hint, "https://shop.example.test/"+
			".well-known/oauth-protected-resource/mcp")
	})

	t.Run("a scope not granted", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"detail": "Missing scope."}`))
			}))
		t.Cleanup(srv.Close)
		h := &handlers{deps: Deps{Site: testSite, Django: django.New(
			srv.URL+"/api/v1", "api.example.test", "secret", 5*time.Second,
			slog.New(slog.DiscardHandler), nil)}}
		linked := identity.NewContext(store,
			&identity.Linked{Bearer: "linked-token"})

		_, _, err := h.myLoyaltyPoints(linked, nil, struct{}{})

		var got *ToolError
		require.ErrorAs(t, err, &got)
		assert.Equal(t, "not_granted", got.Code)
		assert.Contains(t, got.Hint, "loyalty:read")
	})
}

func TestMyFavouritesPricing(t *testing.T) {
	var auth string
	h := fixtureHandlers(t, "agent_favourites.json", func(r *http.Request) {
		auth = r.Header.Get("Authorization")
	})
	ctx := identity.NewContext(
		tenant.NewContext(context.Background(), cartTenant()),
		&identity.Linked{Bearer: "linked-token"})

	res, out, err := h.myFavourites(ctx, nil, struct{}{})
	require.NoError(t, err)
	assert.Equal(t, "Bearer linked-token", auth)
	require.Len(t, out.Favourites, 3)

	announced := out.Favourites[0]
	assert.Equal(t, "428.72", announced.EffectivePrice)
	assert.Equal(t, "476.35", announced.PriorPrice)
	assert.Equal(t, "10", announced.AnnouncedDiscountPercent)
	assert.Equal(t, "EUR", announced.Currency)

	plain := out.Favourites[1]
	assert.Equal(t, "17.91", plain.EffectivePrice)
	assert.Empty(t, plain.PriorPrice)
	assert.Empty(t, plain.AnnouncedDiscountPercent)

	assert.Empty(t, out.Favourites[2].EffectivePrice)

	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, "- Pressure Cooker Final (product 6125): "+
		"428.72 EUR (VAT included), 10% off 476.35 EUR, its lowest "+
		"price in the 30 days before the reduction, in stock")
	assert.Contains(t, text, "- Multivitamin Supplements (product 2): "+
		"17.91 EUR (VAT included), OUT OF STOCK")
	assert.Contains(t, text, "- Ceramic Teapot (product 37): "+
		"price being updated, see the product page, in stock")
}
