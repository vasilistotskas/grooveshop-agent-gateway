package checkout

import (
	"context"
	"encoding/json"
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
		5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)),
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
