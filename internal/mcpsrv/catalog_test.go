package mcpsrv

import (
	"context"
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
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// fixtureHandlers answers every Django call with one recorded fixture,
// passing each request to seen first.
func fixtureHandlers(
	t *testing.T, name string, seen func(*http.Request),
) *handlers {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(
		"..", "..", "testdata", "fixtures", "django", name))
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen(r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
	dj := django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics())
	return &handlers{deps: Deps{Site: testSite, Django: dj}}
}

// searchHandlers serves the recorded search fixture and reports the sort
// value each request carried upstream.
func searchHandlers(t *testing.T) (*handlers, *[]string) {
	t.Helper()
	var sorts []string
	h := fixtureHandlers(t, "search_product.json", func(r *http.Request) {
		sorts = append(sorts, r.URL.Query().Get("sort"))
	})
	return h, &sorts
}

func TestSearchProductsPricing(t *testing.T) {
	h, _ := searchHandlers(t)
	ctx := tenant.NewContext(context.Background(), cartTenant())

	_, out, err := h.searchProducts(ctx, nil, SearchProductsIn{Query: "phone"})
	require.NoError(t, err)
	require.Len(t, out.Products, 2)

	announced := out.Products[0]
	assert.Equal(t, "934.61", announced.EffectivePrice)
	assert.Equal(t, "983.8", announced.PriorPrice)
	assert.Equal(t, "5", announced.AnnouncedDiscountPercent)

	// A 30% markdown upstream, but no prior price: nothing is announced.
	plain := out.Products[1]
	assert.Equal(t, "125.78", plain.EffectivePrice)
	assert.Empty(t, plain.PriorPrice)
	assert.Empty(t, plain.AnnouncedDiscountPercent)
}

// A product's rating is made of its reviews, so a store with reviews off
// serves none.
func TestSearchProductsRatingNeedsReviews(t *testing.T) {
	h, _ := searchHandlers(t)
	tn := cartTenant()

	tn.Features.ProductReviews = true
	_, out, err := h.searchProducts(tenant.NewContext(context.Background(), tn),
		nil, SearchProductsIn{Query: "phone"})
	require.NoError(t, err)
	require.NotEmpty(t, out.Products)
	assert.Equal(t, "0.0", out.Products[0].Rating)

	tn.Features.ProductReviews = false
	_, out, err = h.searchProducts(tenant.NewContext(context.Background(), tn),
		nil, SearchProductsIn{Query: "phone"})
	require.NoError(t, err)
	require.NotEmpty(t, out.Products)
	for _, p := range out.Products {
		assert.Empty(t, p.Rating)
	}
}

func TestSearchProductsSort(t *testing.T) {
	ctx := tenant.NewContext(context.Background(), cartTenant())

	for _, sort := range []string{"", "effectivePrice", "-effectivePrice"} {
		h, sorts := searchHandlers(t)
		_, _, err := h.searchProducts(ctx, nil,
			SearchProductsIn{Query: "phone", Sort: sort})
		require.NoError(t, err, sort)
		assert.Equal(t, []string{sort}, *sorts)
	}

	for _, sort := range []string{"finalPrice", "-finalPrice", "price"} {
		h, sorts := searchHandlers(t)
		_, _, err := h.searchProducts(ctx, nil,
			SearchProductsIn{Query: "phone", Sort: sort})
		require.Error(t, err, sort)
		assert.Contains(t, err.Error(), "effectivePrice")
		assert.Empty(t, *sorts, "a refused sort never reaches Django")
	}
}

func TestPriceOf(t *testing.T) {
	cases := []struct {
		name                  string
		in                    django.Pricing
		effective, prior, pct string
		note                  string
	}{
		{
			name:      "announced reduction",
			in:        django.Pricing{EffectivePrice: "27.00", PriorPrice: "30.00", AnnouncedDiscountPercent: "10"},
			effective: "27.00", prior: "30.00", pct: "10",
			note: "27.00 EUR (VAT included), 10% off 30.00 EUR, its lowest " +
				"price in the 30 days before the reduction",
		},
		{
			name:      "promotion-lowered price, not announceable",
			in:        django.Pricing{EffectivePrice: "27.00"},
			effective: "27.00",
			note:      "27.00 EUR (VAT included)",
		},
		{
			name: "price not recorded yet",
			in:   django.Pricing{},
			note: "price being updated, see the product page",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := priceOf(tc.in, "EUR")
			assert.Equal(t, "EUR", s.Currency)
			assert.Equal(t, tc.effective, s.EffectivePrice)
			assert.Equal(t, tc.prior, s.PriorPrice)
			assert.Equal(t, tc.pct, s.AnnouncedDiscountPercent)
			assert.Equal(t, tc.note, priceNote(&s))
		})
	}
}

// testSite is a storefront built with el as its default language.
var testSite = storefront.NewSite("el")
