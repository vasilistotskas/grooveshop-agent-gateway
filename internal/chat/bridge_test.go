package chat

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcpsrv"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// offeredNames lists the tools the bridge offers the model for a store
// with features, over the shared server that carries every tool.
func offeredNames(t *testing.T, features django.StoreFeatures) []string {
	t.Helper()
	server := mcpsrv.NewServer(mcpsrv.Deps{Version: "test"},
		mcpsrv.BridgeIdentity, mcpsrv.EveryFeature)
	br, err := newBridge(context.Background(), server)
	require.NoError(t, err)
	t.Cleanup(br.close)
	tools, err := br.tools(context.Background(), features)
	require.NoError(t, err)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.GetFunction().Name)
	}
	return names
}

// The bridge shares one server across stores, so it leaves out a tool
// whose feature the store switched off — and only an offered tool is
// callable, so the model cannot reach it either.
func TestBridgeOffersOnlyTheStoreTools(t *testing.T) {
	every := offeredNames(t, mcpsrv.EveryFeature)
	assert.Contains(t, every, "get_product_reviews")

	noReviews := mcpsrv.EveryFeature
	noReviews.ProductReviews = false
	offered := offeredNames(t, noReviews)
	assert.NotContains(t, offered, "get_product_reviews")
	assert.Len(t, offered, len(every)-1)
}

// A cart tool that fails reaches the model as a failure, with the cause
// the tool knows, and the turn does not tell the widget the cart
// changed: the assistant had claimed adds the shopper's cart never got.
// Django adds to a new guest cart when it cannot reach the one named (a
// signed-in shopper's cart is one) and answers 201 for it, as here.
func TestBridgeReportsAFailedCartToolAsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost &&
				strings.HasSuffix(r.URL.Path, "/cart/item") {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id": 7,
					"cartId": "0b7d5a52-8f6e-4c1d-9d2a-5e4f3c2b1a09",
					"product": {"id": 64}, "quantity": 1,
					"finalPrice": 9.9, "totalPrice": 9.9}`))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := mcpsrv.NewServer(mcpsrv.Deps{Version: "test",
		Django: django.New(srv.URL+"/api/v1", "api.example.test", "secret",
			5*time.Second, log, obs.NewMetrics())},
		mcpsrv.BridgeIdentity, mcpsrv.EveryFeature)
	ctx := tenant.NewContext(context.Background(), testTenant())
	br, err := newBridge(ctx, server)
	require.NoError(t, err)
	t.Cleanup(br.close)
	_, err = br.tools(ctx, mcpsrv.EveryFeature)
	require.NoError(t, err)

	text, _, err := br.call(ctx, "add_to_cart", map[string]any{
		"cartId": "6f1c1f3e-2d6a-4b0f-9a3e-2f6d6c1b9a10", "productId": 64})

	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(text, "ERROR: "), text)
	assert.Contains(t, text, "the product is not in it")
	_, mutated := br.cartState()
	assert.False(t, mutated)
}
