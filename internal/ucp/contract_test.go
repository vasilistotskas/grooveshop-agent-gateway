package ucp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// vendoredLoader resolves https://ucp.dev/schemas/* refs against the
// vendored spec snapshot so contract tests never touch the network.
type vendoredLoader struct{ root string }

func (l vendoredLoader) Load(url string) (any, error) {
	const prefix = "https://ucp.dev/schemas/"
	if !strings.HasPrefix(url, prefix) {
		return nil, fmt.Errorf("ref outside vendored spec: %s", url)
	}
	rel := filepath.FromSlash(strings.TrimPrefix(url, prefix))
	f, err := os.Open(filepath.Join(l.root, rel))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return jsonschema.UnmarshalJSON(f)
}

func compileUCP(t *testing.T, ref string) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.UseLoader(vendoredLoader{
		root: filepath.Join("..", "..", "testdata", "schemas", "ucp",
			"2026-04-08"),
	})
	schema, err := c.Compile("https://ucp.dev/schemas/" + ref)
	require.NoError(t, err)
	return schema
}

// roundTrip re-decodes a payload the way a platform receives it.
func roundTrip(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	require.NoError(t, err)
	return doc
}

func testTenant() *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			SchemaName:      "public",
			StoreName:       "Test Store",
			DefaultLocale:   "el",
			DefaultCurrency: "EUR",
			PrimaryDomain:   "shop.example.test",
		},
		Domain: "shop.example.test",
	}
}

func testKey(t *testing.T) *SigningKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	kid, err := jwkThumbprint(pub)
	require.NoError(t, err)
	return &SigningKey{Private: priv, Public: pub, KID: kid}
}

func TestProfileMatchesBusinessSchema(t *testing.T) {
	schema := compileUCP(t, "profile.json#/$defs/business_schema")
	profile := BuildProfile(testTenant(), testKey(t))

	require.NoError(t, schema.Validate(roundTrip(t, profile)))

	assert.Equal(t, "https://shop.example.test/mcp",
		profile.UCP.Services["dev.ucp.shopping"][0].Endpoint)
	assert.Equal(t, "mcp",
		profile.UCP.Services["dev.ucp.shopping"][0].Transport)
	assert.Len(t, profile.Keys, 1)
	assert.Equal(t, "OKP", profile.Keys[0]["kty"])
}

// fixtureDjango serves the recorded fixtures BuildCheckout consumes; an
// optional cartFixture overrides the default cart payload.
func fixtureDjango(t *testing.T, cartFixture ...string) *django.Client {
	t.Helper()
	fixture := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
				"fixtures", "django", name))
			require.NoError(t, err)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		}
	}
	cart := "cart_with_items.json"
	if len(cartFixture) > 0 {
		cart = cartFixture[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/cart", fixture(cart))
	mux.HandleFunc("GET /api/v1/pay_way", fixture("pay_way.json"))
	mux.HandleFunc("GET /api/v1/shipping/options",
		fixture("shipping_options.json"))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics())
}

func TestBuildCheckoutMatchesCheckoutSchema(t *testing.T) {
	schema := compileUCP(t, "shopping/checkout.json")
	tn := testTenant()
	b := NewBuilder(
		fixtureDjango(t),
		"https://{assets_host}/img/{path}.webp",
		"assets.platform.test",
	)

	newSession := func(status checkout.Status) *checkout.Session {
		s := checkout.NewSession("public", tn.Domain, "ucp",
			"29eb4495-e018-45e7-b59c-6646302bd4ef")
		s.Status = status
		return s
	}

	t.Run("incomplete session", func(t *testing.T) {
		s := newSession(checkout.StatusIncomplete)
		payload, err := b.BuildCheckout(context.Background(), tn, s)
		require.NoError(t, err)
		require.NoError(t, schema.Validate(roundTrip(t, payload)))

		// Fixture cart: one line, 2 × 464.68 EUR = 929.36 → minor units.
		require.Len(t, payload.LineItems, 1)
		assert.EqualValues(t, 46468, payload.LineItems[0].Item.Price)
		assert.EqualValues(t, 92936, payload.Totals[0].Amount)
		assert.NotEmpty(t, payload.Messages, "missing-input info messages")
	})

	t.Run("escalated session carries continue_url", func(t *testing.T) {
		s := newSession(checkout.StatusRequiresEscalation)
		s.PaymentURL = "https://www.vivapayments.com/web/checkout?ref=42"
		payload, err := b.BuildCheckout(context.Background(), tn, s)
		require.NoError(t, err)
		require.NoError(t, schema.Validate(roundTrip(t, payload)))
		assert.Equal(t, s.PaymentURL, payload.ContinueURL)
	})

	t.Run("coupon cart emits a negative discount totals line",
		func(t *testing.T) {
			couponBuilder := NewBuilder(
				fixtureDjango(t, "cart_with_coupon.json"),
				"https://{assets_host}/img/{path}.webp",
				"assets.platform.test",
			)
			s := newSession(checkout.StatusIncomplete)
			payload, err := couponBuilder.BuildCheckout(
				context.Background(), tn, s)
			require.NoError(t, err)
			require.NoError(t, schema.Validate(roundTrip(t, payload)))

			// Fixture: 929.36 subtotal, 92.94 promotion discount.
			byType := map[string]int64{}
			for _, total := range payload.Totals {
				byType[total.Type] = total.Amount
			}
			assert.EqualValues(t, 92936, byType["subtotal"])
			assert.EqualValues(t, -9294, byType["discount"],
				"UCP discount totals are strictly negative")
			assert.EqualValues(t, 83642, byType["total"])
		})

	t.Run("completed session carries order confirmation", func(t *testing.T) {
		s := newSession(checkout.StatusCompleted)
		s.OrderID = 684
		s.OrderUUID = "b9be45e5-6062-4976-ae7b-2c31eb2ad689"
		payload, err := b.BuildCheckout(context.Background(), tn, s)
		require.NoError(t, err)
		require.NoError(t, schema.Validate(roundTrip(t, payload)))
		require.NotNil(t, payload.Order)
		assert.Equal(t,
			"https://shop.example.test/checkout/success/"+s.OrderUUID,
			payload.Order.PermalinkURL)
	})
}
