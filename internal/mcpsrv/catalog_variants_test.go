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
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// A sibling variant is listed with its own price, read from the variants
// payload, so an agent comparing colours or sizes needs no get_product per
// variant.
func TestGetProductListsVariantPrices(t *testing.T) {
	detail, err := os.ReadFile(filepath.Join(
		"..", "..", "testdata", "fixtures", "django", "product_detail.json"))
	require.NoError(t, err)
	variants := `{"axes": [], "variants": [
		{"id": 1, "translations": {}, "slug": "self", "active": true,
		 "stock": 3, "mainImagePath": "", "effectivePrice": 464.68,
		 "priorPrice": null, "announcedDiscountPercent": null},
		{"id": 2, "translations": {"el": {"name": "Μπλε"}}, "slug": "blue",
		 "active": true, "stock": 5, "mainImagePath": "",
		 "effectivePrice": 27.00, "priorPrice": 30.00,
		 "announcedDiscountPercent": 10}]}`

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/variants") {
				_, _ = w.Write([]byte(variants))
				return
			}
			_, _ = w.Write(detail)
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
	h := &handlers{deps: Deps{Site: testSite, Django: django.New(srv.URL+"/api/v1",
		"api.example.test", "secret", 5*time.Second, log, obs.NewMetrics())}}
	ctx := tenant.NewContext(context.Background(), cartTenant())

	_, out, err := h.getProduct(ctx, nil, GetProductIn{ProductID: 1})
	require.NoError(t, err)

	require.Len(t, out.Variants, 1, "the product itself is not its own variant")
	assert.Equal(t, int64(2), out.Variants[0].ID)
	assert.Equal(t, Price{
		EffectivePrice: "27.00", PriorPrice: "30.00",
		AnnouncedDiscountPercent: "10", Currency: "EUR",
	}, out.Variants[0].Price)
}
