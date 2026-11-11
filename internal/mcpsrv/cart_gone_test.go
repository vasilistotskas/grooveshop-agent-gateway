package mcpsrv

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// goneCartID names a guest cart the store deleted.
const goneCartID = "6f1c1f3e-2d6a-4b0f-9a3e-2f6d6c1b9a10"

// goneCartDjango answers every cart call as Django answers one naming a
// guest cart it deleted: on a new guest cart (the fixture's), and with a
// 404 for a line, which the new cart does not hold.
func goneCartDjango(t *testing.T, s surface) *handlers {
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
			line := strings.Contains(r.URL.Path, "/cart/item/")
			switch {
			case line && r.Method == http.MethodDelete:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"detail": "Not found."}`))
			case line || strings.HasSuffix(r.URL.Path, "/cart/item"):
				_, _ = w.Write(read("cart_item_created.json"))
			default:
				_, _ = w.Write(read("cart_with_items.json"))
			}
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.DiscardHandler)
	return &handlers{surface: s, deps: Deps{Site: testSite,
		Django: django.New(srv.URL+"/api/v1", "api.example.test", "secret",
			5*time.Second, log, nil)}}
}

// goneCartCalls are the cart tools, each naming the gone cart.
var goneCartCalls = map[string]func(*handlers, context.Context) error{
	"get_cart": func(h *handlers, ctx context.Context) error {
		_, _, err := h.getCart(ctx, nil, GetCartIn{CartID: goneCartID})
		return err
	},
	"add_to_cart": func(h *handlers, ctx context.Context) error {
		_, _, err := h.addToCart(ctx, nil,
			AddToCartIn{CartID: goneCartID, ProductID: 1})
		return err
	},
	"update_cart_item": func(h *handlers, ctx context.Context) error {
		_, _, err := h.updateCartItem(ctx, nil,
			UpdateCartItemIn{CartID: goneCartID, ItemID: 410, Quantity: 2})
		return err
	},
	"remove_cart_item": func(h *handlers, ctx context.Context) error {
		_, _, err := h.removeCartItem(ctx, nil,
			RemoveCartItemIn{CartID: goneCartID, ItemID: 410})
		return err
	},
	"get_checkout_link": func(h *handlers, ctx context.Context) error {
		_, _, err := h.getCheckoutLink(ctx, nil,
			CheckoutLinkIn{CartID: goneCartID})
		return err
	},
	"get_shipping_options": func(h *handlers, ctx context.Context) error {
		_, _, err := h.getShippingOptions(ctx, nil,
			ShippingOptionsIn{CountryCode: "GR", CartID: goneCartID})
		return err
	},
	"apply_coupon": func(h *handlers, ctx context.Context) error {
		_, _, err := h.applyCoupon(ctx, nil,
			ApplyCouponIn{CartID: goneCartID, Code: "SUMMER"})
		return err
	},
	"remove_coupon": func(h *handlers, ctx context.Context) error {
		_, _, err := h.removeCoupon(ctx, nil,
			RemoveCouponIn{CartID: goneCartID})
		return err
	},
}

// In the chat the cart is the shopper's own browser cart, which a cart
// the model created would never replace: a gone one is reported as
// unreachable, with nothing changed, and the shopper told to reload.
func TestChatCartToolsNeverAnswerAGoneCartWithCreateCart(t *testing.T) {
	for name, call := range goneCartCalls {
		t.Run(name, func(t *testing.T) {
			h := goneCartDjango(t, chatSurface)
			ctx, activity := listening(
				tenant.NewContext(context.Background(), cartTenant()))

			err := call(h, ctx)

			var got *ToolError
			require.ErrorAs(t, err, &got)
			assert.NotContains(t, got.Hint, "create_cart")
			assert.False(t, activity.Mutated())
			if name == "remove_cart_item" {
				// Django finds no such line on the new cart: the model
				// is sent to get_cart, which reports the cart gone.
				assert.Contains(t, got.Hint, "get_cart")
				return
			}
			assert.Equal(t, &ToolError{Code: codeCartUnreachable,
				Hint: shopperCartUnreachable}, got)
			assert.Contains(t, got.Hint, "reload the page")
		})
	}
}

// On /mcp the agent owns its cart id: a gone cart is not found, and the
// agent may create another.
func TestMCPCartToolsAnswerAGoneCartWithCreateCart(t *testing.T) {
	for name, call := range goneCartCalls {
		if name == "remove_cart_item" || name == "update_cart_item" ||
			name == "apply_coupon" || name == "remove_coupon" {
			// A line tool's not-found names the line; the coupon tools
			// are the chat's alone.
			continue
		}
		t.Run(name, func(t *testing.T) {
			h := goneCartDjango(t, mcpSurface)
			ctx := tenant.NewContext(context.Background(), cartTenant())

			err := call(h, ctx)

			var got *ToolError
			require.ErrorAs(t, err, &got)
			assert.Equal(t, codeNotFound, got.Code)
			assert.Contains(t, got.Hint, "create_cart")
		})
	}
}
