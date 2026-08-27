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
			"2026-08-25"),
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
			// The fixture store accepts cash on delivery, so the
			// profile advertises a payment handler.
			AgentPaymentInstruments: []string{"cash_on_delivery"},
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
	profile := BuildProfile(testTenant(), testKey(t), "production")

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
		"production",
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
				"production",
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

// TestProfileAdvertisesResolvableUCPDocuments locks the shape of every
// spec and schema URL the profile publishes. Both properties matter and
// neither is expressible in the JSON Schema: the URLs must sit under the
// versioned release path, because the unversioned
// https://ucp.dev/schemas/... form 404s and a platform drops any entity
// whose schema it cannot fetch; and they must stay on ucp.dev, because
// authority binding makes a platform reject a dev.ucp.* entity served from
// a non-name-aligned origin.
func TestProfileAdvertisesResolvableUCPDocuments(t *testing.T) {
	profile := BuildProfile(testTenant(), testKey(t), "production")
	wantPrefix := "https://ucp.dev/" + Version + "/"

	urls := map[string]string{}
	for name, services := range profile.UCP.Services {
		for i, s := range services {
			urls[fmt.Sprintf("services[%s][%d].spec", name, i)] = s.Spec
			urls[fmt.Sprintf("services[%s][%d].schema", name, i)] =
				s.Schema
		}
	}
	for name, caps := range profile.UCP.Capabilities {
		for i, c := range caps {
			urls[fmt.Sprintf("capabilities[%s][%d].spec", name, i)] = c.Spec
			urls[fmt.Sprintf("capabilities[%s][%d].schema", name, i)] = c.Schema
		}
	}
	require.NotEmpty(t, urls)

	for field, url := range urls {
		require.NotEmpty(t, url, "%s must be published", field)
		assert.True(t, strings.HasPrefix(url, wantPrefix),
			"%s = %q must sit under %s", field, url, wantPrefix)
	}
}

// A checkout's handler declaration is authoritative, and whether it
// carries an instrument decides whether the agent may complete. These
// cases pin both, and each payload is validated against the real
// checkout schema so the escalation branch cannot drift out of spec.
func TestBuildCheckoutPaymentParity(t *testing.T) {
	schema := compileUCP(t, "shopping/checkout.json")
	dj := fixtureDjango(t)

	build := func(t *testing.T, tn *tenant.Tenant, st checkout.Status,
		paymentURL string,
	) *Checkout {
		t.Helper()
		b := NewBuilder(dj, "https://{assets_host}/img/{path}.webp",
			"assets.platform.test", "production")
		s := checkout.NewSession(tn.SchemaName, tn.Domain, "ucp",
			"29eb4495-e018-45e7-b59c-6646302bd4ef")
		s.Status = st
		s.PaymentURL = paymentURL
		payload, err := b.BuildCheckout(context.Background(), tn, s)
		require.NoError(t, err)
		require.NoError(t, schema.Validate(roundTrip(t, payload)))
		return payload
	}

	t.Run("agent-completable store stays ready and declares instruments",
		func(t *testing.T) {
			out := build(t, testTenant(), checkout.StatusReadyForComplete, "")

			assert.Equal(t, string(checkout.StatusReadyForComplete),
				out.Status)

			h := out.UCP.PaymentHandlers[HandlerName][0]
			require.Len(t, h.AvailableInstruments, 1)
			assert.Equal(t, InstrumentCashOnDelivery,
				h.AvailableInstruments[0].Type)
			// A response declares resolved state, not documents.
			assert.Empty(t, h.Spec)
			assert.Empty(t, h.Schema)
			assert.Equal(t, "on_delivery", h.Config["settlement"])
		})

	t.Run("online-only store keeps readiness so complete can run",
		func(t *testing.T) {
			// UCP readiness means "inputs collected, call complete". The
			// escalation belongs to completion, which returns the PSP's
			// own continue_url; reporting it here would send the agent to
			// a browser and no order would ever be placed.
			tn := testTenant()
			tn.AgentPaymentInstruments = []string{"viva_wallet"}

			out := build(t, tn, checkout.StatusReadyForComplete, "")

			assert.Equal(t, string(checkout.StatusReadyForComplete),
				out.Status)
			assert.Empty(t, out.UCP.PaymentHandlers,
				"registry present but empty: nothing an agent can settle")
			assert.Empty(t, out.ContinueURL,
				"no handoff until completion produces one")
		})

	t.Run("hosted payment page wins as the handoff target",
		func(t *testing.T) {
			const psp = "https://www.vivapayments.com/web/checkout?ref=42"
			out := build(t, testTenant(),
				checkout.StatusRequiresEscalation, psp)

			assert.Equal(t, psp, out.ContinueURL)
		})
}

// The order object is what a platform reads post-purchase, so it is
// validated against the real schema rather than field-by-field.
func TestBuildOrderMatchesOrderSchema(t *testing.T) {
	schema := compileUCP(t, "shopping/order.json")
	tn := testTenant()

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata",
		"fixtures", "django", "order_by_uuid.json"))
	require.NoError(t, err)
	var order django.Order
	require.NoError(t, json.Unmarshal(raw, &order))

	out, err := BuildOrder(tn, &order, "chk_abc123")
	require.NoError(t, err)
	require.NoError(t, schema.Validate(roundTrip(t, out)))

	assert.Equal(t, "chk_abc123", out.CheckoutID)
	assert.Equal(t, order.UUID, out.ID)
	assert.Contains(t, out.PermalinkURL, "/checkout/success/"+order.UUID)
	require.NotEmpty(t, out.LineItems)

	// Exactly one subtotal and one total, as the schema demands.
	counts := map[string]int{}
	for _, tot := range out.Totals {
		counts[tot.Type]++
	}
	assert.Equal(t, 1, counts["subtotal"])
	assert.Equal(t, 1, counts["total"])
}

// checkout_id is required, so a caller that cannot name the checkout must
// be refused rather than handed an order with an empty link.
func TestBuildOrderRefusesWithoutACheckout(t *testing.T) {
	_, err := BuildOrder(testTenant(), &django.Order{UUID: "o-1"}, "")
	assert.ErrorContains(t, err, "no known checkout session")
}

func TestOrderLineStatusFollowsTheSchemaDefinition(t *testing.T) {
	assert.Equal(t, "removed", orderLineStatus(0, 0))
	assert.Equal(t, "fulfilled", orderLineStatus(3, 3))
	assert.Equal(t, "partial", orderLineStatus(3, 1))
	assert.Equal(t, "processing", orderLineStatus(3, 0))
}
