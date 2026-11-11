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
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcptool"
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

			var got *mcptool.ToolError
			require.ErrorAs(t, err, &got)
			assert.NotContains(t, got.Hint, "create_cart")
			assert.False(t, activity.Mutated())
			if name == "remove_cart_item" {
				// Django finds no such line on the new cart: the model
				// is sent to get_cart, which reports the cart gone.
				assert.Contains(t, got.Hint, "get_cart")
				return
			}
			assert.Equal(t, &mcptool.ToolError{Code: mcptool.CodeCartUnreachable,
				Hint: shopperCartUnreachable}, got)
			assert.Contains(t, got.Hint, "reload the page")
		})
	}
}

// A bare 404 is a Django feature gate's (tenant/permissions.py raises a
// NotFound with no code); a cart Django does not have is cart_not_found
// (cart/views/cart.py).
const (
	bare404     = `{"detail": "Not found."}`
	cartGone404 = `{"detail": "Cart not found", "code": "cart_not_found"}`
)

// notFoundDjango answers every call with a bare 404.
func notFoundDjango(t *testing.T, s surface) *handlers {
	t.Helper()
	return refusingDjango(t, s, http.StatusNotFound, bare404)
}

// refusingDjango answers every call with status and body.
func refusingDjango(
	t *testing.T, s surface, status int, body string,
) *handlers {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	t.Cleanup(srv.Close)
	return &handlers{surface: s, deps: Deps{Site: testSite,
		Django: django.New(srv.URL+"/api/v1", "api.example.test", "secret",
			5*time.Second, slog.New(slog.DiscardHandler), nil)}}
}

// cartScoped are the cart tools whose 404 can be the cart itself.
var cartScoped = []string{"get_cart", "get_checkout_link",
	"get_shipping_options", "apply_coupon", "remove_coupon"}

// mcpServes reports whether /mcp serves a cartScoped tool: the coupon
// tools are the chat's alone.
func mcpServes(name string) bool {
	return name != "apply_coupon" && name != "remove_coupon"
}

// A cart the chat names that Django does not have (cart_not_found, or a
// 403, which reads as a 404) is the shopper's own browser cart gone:
// unreachable, reload, never create_cart, which the chat does not serve.
func TestChatCartAnswered404IsUnreachable(t *testing.T) {
	type answer struct {
		status int
		body   string
	}
	for _, name := range cartScoped {
		for _, a := range []answer{
			{http.StatusNotFound, cartGone404},
			{http.StatusForbidden, `{"detail": "Forbidden."}`},
		} {
			t.Run(name+" "+http.StatusText(a.status), func(t *testing.T) {
				h := refusingDjango(t, chatSurface, a.status, a.body)
				ctx := tenant.NewContext(context.Background(), cartTenant())

				err := goneCartCalls[name](h, ctx)

				var got *mcptool.ToolError
				require.ErrorAs(t, err, &got)
				assert.Equal(t, &mcptool.ToolError{
					Code: mcptool.CodeCartUnreachable,
					Hint: shopperCartUnreachable}, got)
				assert.NotContains(t, err.Error(), "create_cart")
			})
		}
	}
}

// On /mcp the same cart_not_found is a cart that is gone, which the agent
// may replace with create_cart.
func TestMCPCartAnswered404IsNotFound(t *testing.T) {
	for _, name := range cartScoped {
		if !mcpServes(name) {
			continue
		}
		t.Run(name, func(t *testing.T) {
			h := refusingDjango(t, mcpSurface, http.StatusNotFound,
				cartGone404)
			ctx := tenant.NewContext(context.Background(), cartTenant())

			err := goneCartCalls[name](h, ctx)

			var got *mcptool.ToolError
			require.ErrorAs(t, err, &got)
			assert.Equal(t, mcptool.CodeNotFound, got.Code)
			assert.Equal(t, mcpCartGone, got.Hint)
		})
	}
}

// A bare 404 is a feature the store switched off while this pod still
// offers the tool: on either surface it is not available here, never a
// cart to reload or replace.
func TestCartAnsweredABare404IsAFeatureOff(t *testing.T) {
	for _, s := range []surface{mcpSurface, chatSurface} {
		for _, name := range cartScoped {
			if s == mcpSurface && !mcpServes(name) {
				continue
			}
			t.Run(s.label()+" "+name, func(t *testing.T) {
				h := notFoundDjango(t, s)
				ctx := tenant.NewContext(context.Background(), cartTenant())

				err := goneCartCalls[name](h, ctx)

				var got *mcptool.ToolError
				require.ErrorAs(t, err, &got)
				assert.Equal(t, &mcptool.ToolError{
					Code: mcptool.CodeNotFound, Hint: featureOff}, got)
			})
		}
	}
}

// add_to_cart's 404 is its product's, on either surface: not a cart
// that is gone.
func TestAddToCartAnswered404IsTheProduct(t *testing.T) {
	for _, s := range []surface{mcpSurface, chatSurface} {
		h := notFoundDjango(t, s)
		ctx := tenant.NewContext(context.Background(), cartTenant())

		err := goneCartCalls["add_to_cart"](h, ctx)

		var got *mcptool.ToolError
		require.ErrorAs(t, err, &got)
		assert.Equal(t, mcptool.CodeNotFound, got.Code, s.label())
		assert.Equal(t, "product 1 was not found", got.Hint, s.label())
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

			var got *mcptool.ToolError
			require.ErrorAs(t, err, &got)
			assert.Equal(t, mcptool.CodeNotFound, got.Code)
			assert.Contains(t, got.Hint, "create_cart")
		})
	}
}
