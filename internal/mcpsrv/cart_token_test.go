package mcpsrv

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// fixtureCartID is the cart cart_with_items.json serves.
const fixtureCartID = "29eb4495-e018-45e7-b59c-6646302bd4ef"

// cartHandlers serves GET /cart from the fixture and records the cart
// headers each call sent.
func cartHandlers(
	t *testing.T, status int,
) (*handlers, *[]http.Header) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata",
		"fixtures", "django", "cart_with_items.json"))
	require.NoError(t, err)
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.Header.Clone())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if status == http.StatusOK {
				_, _ = w.Write(body)
				return
			}
			_, _ = w.Write([]byte(`{"detail": "refused"}`))
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &handlers{deps: Deps{Django: django.New(srv.URL+"/api/v1",
		"api.example.test", "secret", 5*time.Second, log, nil)}}, &seen
}

func signedIn(t *testing.T) context.Context {
	t.Helper()
	tok, err := django.ParseCartToken("Zk3n_Q8vX2-rT5yLmW9aB4cD7eF0gH1iJ6k")
	require.NoError(t, err)
	return django.WithCartToken(
		tenant.NewContext(context.Background(), cartTenant()), tok)
}

// A signed-in shopper's cart is theirs already: the link goes straight
// to checkout, after a check through the token, never through an
// X-Cart-Id lookup that cannot see their cart. The link carries no token.
func TestCheckoutLinkForASignedInShopper(t *testing.T) {
	h, seen := cartHandlers(t, http.StatusOK)

	_, out, err := h.getCheckoutLink(signedIn(t), nil, CheckoutLinkIn{})

	require.NoError(t, err)
	assert.Equal(t, "https://shop.example.test/checkout", out.URL)
	require.Len(t, *seen, 1)
	assert.NotEmpty(t, (*seen)[0].Get("X-Cart-Token"))
	assert.Empty(t, (*seen)[0].Get("X-Cart-Id"))
}

// A guest's link still claims the named cart into the browser session.
func TestCheckoutLinkForAGuestClaimsTheCart(t *testing.T) {
	h, seen := cartHandlers(t, http.StatusOK)
	ctx := tenant.NewContext(context.Background(), cartTenant())

	_, out, err := h.getCheckoutLink(ctx, nil,
		CheckoutLinkIn{CartID: fixtureCartID})

	require.NoError(t, err)
	assert.Equal(t, "https://shop.example.test/cart/claim?uuid="+
		fixtureCartID, out.URL)
	require.Len(t, *seen, 1)
	assert.Equal(t, fixtureCartID, (*seen)[0].Get("X-Cart-Id"))

	_, _, err = h.getCheckoutLink(ctx, nil, CheckoutLinkIn{})
	assert.EqualError(t, err, "cartId is required")
}

// A refused token is no link and no "create a new cart" advice: the
// shopper's own cart could not be reached.
func TestCartToolsReportARefusedToken(t *testing.T) {
	h, _ := cartHandlers(t, http.StatusUnauthorized)
	ctx := signedIn(t)

	_, out, err := h.getCheckoutLink(ctx, nil, CheckoutLinkIn{})
	require.Error(t, err)
	assert.Empty(t, out.URL)
	assert.Contains(t, err.Error(), "cart could not be reached")
	assert.NotContains(t, err.Error(), "create_cart")

	_, _, err = h.getCart(ctx, nil, GetCartIn{})
	assert.Contains(t, err.Error(), "cart could not be reached")
}

// The model's cartId is not needed, and not used, for a signed-in cart.
func TestCartToolsNeedNoCartIDWhenSignedIn(t *testing.T) {
	h, seen := cartHandlers(t, http.StatusOK)

	_, out, err := h.getCart(signedIn(t), nil,
		GetCartIn{CartID: "6f1c1f3e-2d6a-4b0f-9a3e-2f6d6c1b9a10"})
	require.NoError(t, err)
	assert.Equal(t, fixtureCartID, out.CartID)
	_, _, err = h.getCart(signedIn(t), nil, GetCartIn{})
	require.NoError(t, err)
	for _, hdr := range *seen {
		assert.Empty(t, hdr.Get("X-Cart-Id"))
	}
}
