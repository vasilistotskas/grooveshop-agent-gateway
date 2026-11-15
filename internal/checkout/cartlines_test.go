package checkout

import (
	"context"
	"encoding/json"
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
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// The lines a UCP or ACP checkout adds book the cart as the agent's, the
// same source its order gets, so the admin reads "AI agent" on both.
func TestSyncCartLinesBooksTheCartAsTheAgents(t *testing.T) {
	const cartID = "29eb4495-e018-45e7-b59c-6646302bd4ef"
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
			"fixtures", "django", name))
		require.NoError(t, err)
		return b
	}
	var adds []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/cart",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(read("cart_empty.json"))
		})
	mux.HandleFunc("POST /api/v1/cart/item",
		func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			adds = append(adds, body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(read("cart_item_created.json"))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	dj := django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, slog.New(slog.DiscardHandler),
		obs.NewMetrics())

	for _, protocol := range []string{ProtocolUCP, ProtocolACP} {
		adds = nil
		require.NoError(t, SyncCartLines(context.Background(), dj,
			pricingTenant(), protocol, cartID,
			[]Line{{ProductID: 7, Quantity: 2}}))
		require.Len(t, adds, 1)
		assert.Equal(t, map[string]any{"agentProtocol": protocol},
			adds[0]["attribution"], protocol)
	}
}

// A product named on several lines is one cart line of their sum, on a
// cart that is new and on one that already holds the product: Django's
// add stacks, and its update sets.
func TestSyncCartLinesSumsTheLinesOfOneProduct(t *testing.T) {
	const cartID = "29eb4495-e018-45e7-b59c-6646302bd4ef"
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
			"fixtures", "django", name))
		require.NoError(t, err)
		return b
	}
	for name, tc := range map[string]struct {
		cart  string
		wrote string
	}{
		// cart_with_items.json holds product 1 on line 410, twice.
		"a new cart":                 {"cart_empty.json", "POST /api/v1/cart/item"},
		"a cart holding the product": {"cart_with_items.json", "PUT /api/v1/cart/item/410"},
	} {
		t.Run(name, func(t *testing.T) {
			var writes []string
			var quantities []any
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet {
						_, _ = w.Write(read(tc.cart))
						return
					}
					var body map[string]any
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					writes = append(writes, r.Method+" "+r.URL.Path)
					quantities = append(quantities, body["quantity"])
					if r.Method == http.MethodPost {
						w.WriteHeader(http.StatusCreated)
					}
					_, _ = w.Write(read("cart_item_created.json"))
				}))
			t.Cleanup(srv.Close)
			dj := django.New(srv.URL+"/api/v1", "api.example.test", "secret",
				5*time.Second, slog.New(slog.DiscardHandler),
				obs.NewMetrics())

			require.NoError(t, SyncCartLines(t.Context(), dj,
				pricingTenant(), ProtocolUCP, cartID, []Line{
					{ProductID: 1, Quantity: 1}, {ProductID: 1, Quantity: 4},
				}))

			assert.Equal(t, []string{tc.wrote}, writes)
			assert.Equal(t, []any{float64(5)}, quantities)
		})
	}
}
