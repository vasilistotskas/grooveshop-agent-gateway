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

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// cartRoutes serves the add and read-back from fixtures, or the status
// each route is given.
func cartRoutes(
	t *testing.T, addStatus, readStatus int,
) *handlers {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
			"fixtures", "django", name))
		require.NoError(t, err)
		return b
	}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			status, body := readStatus, read("cart_with_items.json")
			if r.Method == http.MethodPost {
				status, body = addStatus, read("cart_item_created.json")
			}
			w.WriteHeader(status)
			if status < 300 {
				_, _ = w.Write(body)
			}
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &handlers{deps: Deps{Django: django.New(srv.URL+"/api/v1",
		"api.example.test", "secret", 5*time.Second, log, nil)}}
}

func listening(ctx context.Context) (context.Context, *CartActivity) {
	a := &CartActivity{}
	return WithCartActivity(ctx, a), a
}

// A change the store accepted is a change, even when the cart cannot be
// read back after it: the tool says so, and that it must not be redone,
// instead of claiming nothing changed.
func TestCartChangeWithAFailedReadBackIsReportedAsMade(t *testing.T) {
	for name, readStatus := range map[string]int{
		"token refused on the read-back": http.StatusUnauthorized,
		"store down on the read-back":    http.StatusServiceUnavailable,
	} {
		t.Run(name, func(t *testing.T) {
			h := cartRoutes(t, http.StatusCreated, readStatus)
			ctx, activity := listening(signedIn(t))

			_, _, err := h.addToCart(ctx, nil, AddToCartIn{ProductID: 1})

			require.ErrorIs(t, err, errReadBack)
			assert.NotContains(t, err.Error(), "nothing in it changed")
			assert.True(t, activity.Mutated())
		})
	}
}

// A refusal on the change itself changed nothing, and says so.
func TestCartChangeRefusedByTheStoreIsNoChange(t *testing.T) {
	h := cartRoutes(t, http.StatusUnauthorized, http.StatusOK)
	ctx, activity := listening(signedIn(t))

	_, _, err := h.addToCart(ctx, nil, AddToCartIn{ProductID: 1})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not be reached")
	assert.False(t, activity.Mutated())
}

// create_cart for a signed-in shopper creates nothing: their cart exists,
// and the tool says so instead of announcing a new one.
func TestCreateCartForASignedInShopperCreatesNothing(t *testing.T) {
	h, _ := cartHandlers(t, http.StatusOK)
	ctx, activity := listening(signedIn(t))

	res, out, err := h.createCart(ctx, nil, struct{}{})

	require.NoError(t, err)
	assert.Equal(t, fixtureCartID, out.CartID)
	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, "No cart was created")
	assert.NotContains(t, text, "Created cart")
	assert.False(t, activity.Mutated())

	ctx, activity = listening(
		tenant.NewContext(context.Background(), cartTenant()))
	res, _, err = h.createCart(ctx, nil, struct{}{})
	require.NoError(t, err)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "Created cart")
	assert.True(t, activity.Mutated())
}
