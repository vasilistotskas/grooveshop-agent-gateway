package mcpsrv

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

const fixtureOrderUUID = "b9be45e5-6062-4976-ae7b-2c31eb2ad689"

// ucpMeta names a platform profile served from the recorded fixture.
func ucpMeta(t *testing.T, idem string) *MetaIn {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata",
		"fixtures", "ucp", "platform_profile.json"))
	require.NoError(t, err)
	body := strings.ReplaceAll(string(raw), "{{webhook_url}}",
		"http://127.0.0.1:1/ucp/orders")
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
	t.Cleanup(srv.Close)
	return &MetaIn{UCPAgent: &UCPAgentIn{Profile: srv.URL + "/profile.json"},
		IdempotencyKey: idem}
}

// ucpDjango serves the pay ways and the placed order from their fixtures
// and records every call it gets.
func ucpDjango(t *testing.T) (*django.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls = append(calls, r.Method+" "+r.URL.Path)
			mu.Unlock()
			fixture := map[string]string{
				"/api/v1/pay_way":                        "pay_way.json",
				"/api/v1/order/uuid/" + fixtureOrderUUID: "order_by_uuid.json",
			}[r.URL.Path]
			if r.Method != http.MethodGet || fixture == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			body, err := os.ReadFile(filepath.Join("..", "..", "testdata",
				"fixtures", "django", fixture))
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}))
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
			5*time.Second, slog.New(slog.DiscardHandler), nil),
		func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), calls...)
		}
}

// ucpHandlers serve the UCP tools over dj with a checkout store that
// cannot be reached.
func ucpHandlers(t *testing.T, dj *django.Client) *handlers {
	t.Helper()
	profiles, err := ucp.NewProfileResolver(true, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 20 * time.Millisecond,
		MaxRetries: -1,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	return &handlers{deps: Deps{Site: testSite, Django: dj,
		Profiles: profiles, Checkout: checkout.NewStore(rdb),
		AllowLocalWebhooks: true, Log: slog.New(slog.DiscardHandler)}}
}

// A payment the store refuses is refused before the cart is written:
// create_checkout neither creates a cart nor fills one. (update_checkout
// orders its steps the same way, but takes its session lock in Redis
// first, which this package's tests do not run; the integration suite
// covers it.)
func TestCreateCheckoutRefusesThePaymentBeforeTheCart(t *testing.T) {
	for name, tc := range map[string]struct {
		instrument UCPInstrumentIn
		want       string
	}{
		"another handler's instrument": {UCPInstrumentIn{
			HandlerID: "com.example.elsewhere",
			Type:      ucp.InstrumentCashOnDelivery,
		}, "unknown payment handler_id"},
		"an instrument the store does not offer": {UCPInstrumentIn{
			HandlerID: ucp.HandlerID, Type: "card",
		}, "not available"},
	} {
		t.Run(name, func(t *testing.T) {
			dj, calls := ucpDjango(t)
			h := ucpHandlers(t, dj)
			ctx := tenant.NewContext(context.Background(), cartTenant())

			_, _, err := h.createCheckout(ctx, nil, CreateCheckoutIn{
				Meta: ucpMeta(t, ""),
				Checkout: UCPCheckoutIn{
					LineItems: []UCPLineItemReqIn{{
						Item: UCPItemIn{ID: "2"}, Quantity: 1,
					}},
					Discounts: &UCPDiscountsIn{Codes: []string{"SAVE10"}},
					Payment: &UCPPaymentIn{
						Instruments: []UCPInstrumentIn{tc.instrument},
					},
				},
			})

			require.ErrorContains(t, err, tc.want)
			for _, call := range calls() {
				assert.NotContains(t, call, "/api/v1/cart")
			}
		})
	}
}

// A checkout store that cannot be reached is the store being unavailable,
// the same ToolError at every tool that reads or writes it.
func TestCheckoutStoreOutageIsUnavailable(t *testing.T) {
	id := uuid.NewString()
	for name, call := range map[string]func(
		context.Context, *handlers, *testing.T,
	) error{
		"create_checkout saves the session": func(
			ctx context.Context, h *handlers, t *testing.T,
		) error {
			_, _, err := h.createCheckout(ctx, nil, CreateCheckoutIn{
				Meta: ucpMeta(t, ""), CartID: fixtureCartID,
			})
			return err
		},
		"get_checkout reads it": func(
			ctx context.Context, h *handlers, t *testing.T,
		) error {
			_, _, err := h.getCheckout(ctx, nil,
				GetCheckoutIn{Meta: ucpMeta(t, ""), ID: id})
			return err
		},
		"update_checkout locks it": func(
			ctx context.Context, h *handlers, t *testing.T,
		) error {
			_, _, err := h.updateCheckout(ctx, nil,
				UpdateCheckoutIn{Meta: ucpMeta(t, ""), ID: id})
			return err
		},
		"complete_checkout locks it": func(
			ctx context.Context, h *handlers, t *testing.T,
		) error {
			_, _, err := h.completeCheckout(ctx, nil, CompleteCheckoutIn{
				Meta: ucpMeta(t, uuid.NewString()), ID: id,
			})
			return err
		},
		"cancel_checkout locks it": func(
			ctx context.Context, h *handlers, t *testing.T,
		) error {
			_, _, err := h.cancelCheckout(ctx, nil, CancelCheckoutIn{
				Meta: ucpMeta(t, uuid.NewString()), ID: id,
			})
			return err
		},
		"get_order reads the order link": func(
			ctx context.Context, h *handlers, t *testing.T,
		) error {
			_, _, err := h.getOrder(ctx, nil,
				GetOrderIn{Meta: ucpMeta(t, ""), ID: fixtureOrderUUID})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			dj, _ := ucpDjango(t)
			ctx := tenant.NewContext(context.Background(), cartTenant())

			err := call(ctx, ucpHandlers(t, dj), t)

			require.ErrorIs(t, err, errCheckoutUnavailable)
			var got *ToolError
			require.ErrorAs(t, err, &got)
			assert.Equal(t, "unavailable", got.Code)
			assert.Equal(t, unavailableRetryAfter, got.RetryAfter)
		})
	}
}
