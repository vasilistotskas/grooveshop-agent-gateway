package ucp

import (
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
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// checkoutCaps is the declaration a negotiated checkout response carries,
// with the discount extension; plainCaps is one without it.
var (
	checkoutCaps = map[string][]CapabilityRef{
		CapabilityCheckout: {{Version: Version}},
		CapabilityDiscount: {{Version: Version}},
	}
	plainCaps = map[string][]CapabilityRef{
		CapabilityCheckout: {{Version: Version}},
	}
)

func compileUCP(t *testing.T, ref string) *jsonschema.Schema {
	t.Helper()
	schema, err := CompileSpec(ref)
	require.NoError(t, err)
	return schema
}

// checkoutSchema is checkout.json composed with the discount extension,
// as the extension's own schema composes it.
func checkoutSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	return compileUCP(t, "shopping/discount.json#/$defs/dev.ucp.shopping.checkout")
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
		SchemaName:      "public",
		StoreName:       "Test Store",
		DefaultLocale:   "el",
		DefaultCurrency: "EUR",
		PrimaryDomain:   "shop.example.test",
		// The fixture store accepts cash on delivery, so the
		// profile advertises a payment handler.
		AgentPaymentInstruments: []string{"cash_on_delivery"},
		Domain:                  "shop.example.test",
	}
}

// A platform that fetched the profile on another of the store's hosts must
// still be led to the server its OAuth metadata names.
func TestProfileEndpointNamesThePrimaryDomain(t *testing.T) {
	tn := testTenant()
	tn.Domain = "www.shop.example.test"
	profile := BuildProfile(tn, testKey(t), "production")
	assert.Equal(t, "https://shop.example.test/mcp",
		profile.UCP.Services["dev.ucp.shopping"][0].Endpoint)
}

func testKey(t *testing.T) *SigningKey {
	t.Helper()
	raw, err := newScalar()
	require.NoError(t, err)
	key, err := SigningKeyFromScalar(raw)
	require.NoError(t, err)
	return key
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
	assert.Equal(t, "EC", profile.Keys[0]["kty"])
	assert.Equal(t, "ES256", profile.Keys[0]["alg"])
}

// fixtureDjango serves the recorded fixtures BuildCheckout consumes; an
// optional cartFixture overrides the default cart payload.
func fixtureDjango(t *testing.T, cartFixture ...string) *django.Client {
	t.Helper()
	fixture := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
				"fixtures", "django", name))
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
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
	mux.HandleFunc("GET /api/v1/agent/internal/order/b9be45e5-6062-4976-ae7b-2c31eb2ad689",
		fixture("order_by_uuid.json"))
	for uuid, o := range placedOrders {
		mux.HandleFunc("GET /api/v1/agent/internal/order/"+uuid,
			func(w http.ResponseWriter, _ *http.Request) {
				b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
					"fixtures", "django", "order_awaiting_payment.json"))
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(o.Replace(string(b))))
			})
	}
	mux.HandleFunc("GET /api/v1/pay_way/1", fixture("pay_way_1.json"))
	mux.HandleFunc("GET /api/v1/pay_way/2", fixture("pay_way_2.json"))
	mux.HandleFunc("GET /api/v1/shipping/options",
		fixture("shipping_options.json"))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	log := slog.New(slog.DiscardHandler)
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics())
}

// The orders fixtureDjango serves besides the recorded ones: the
// recorded online order awaiting its payment, as it reads at another
// moment. Their uuids name the moment.
const (
	// Its link can still be paid: its hold and its decision are ahead.
	orderPayable = "0d6f4c2e-7a51-4b8e-9c3d-2f1e5a6b7c80"
	// As recorded: its hold ran out long ago.
	orderHoldPassed = "1e7a5d3f-8b62-4c9f-8d4e-3a2f6b7c8d91"
	// Its last payment attempt failed: its link can still be paid.
	orderPaymentFailed = "2f8b6e4a-9c73-4daf-9e5f-4b3a7c8d9ea2"
	// The provider is processing its payment: nothing will decide it.
	orderProcessing = "4bad8a6c-be95-4fc1-b07b-6d5c9eb0f0c4"
	// Canceled, and paid after that.
	orderCanceledPaid = "3a9c7f5b-ad84-4eb0-af6a-5c4b8d9eafb3"
	// Canceled unpaid: Django settles its payment as canceled.
	orderCanceledUnpaid = "5cbe9b7d-cfa6-40d2-81b7-7e6daf0c1fd5"
)

// The instants the payable order reads: its hold and the decision.
var (
	payableHold    = time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)
	payableDecided = time.Now().Add(40 * time.Minute).UTC().Truncate(time.Second)
)

var placedOrders = map[string]*strings.Replacer{
	orderPayable: strings.NewReplacer(
		"2026-05-21T21:44:59.047689+03:00", payableHold.Format(time.RFC3339),
		"2026-05-21T22:05:00+03:00", payableDecided.Format(time.RFC3339)),
	orderHoldPassed: strings.NewReplacer(),
	orderPaymentFailed: strings.NewReplacer(
		`"paymentStatus":"PENDING"`, `"paymentStatus":"FAILED"`,
		"2026-05-21T21:44:59.047689+03:00", payableHold.Format(time.RFC3339),
		"2026-05-21T22:05:00+03:00", payableDecided.Format(time.RFC3339)),
	orderCanceledUnpaid: strings.NewReplacer(
		`"status":"PENDING","statusDisplay"`,
		`"status":"CANCELED","statusDisplay"`,
		`"paymentStatus":"PENDING"`, `"paymentStatus":"CANCELED"`,
		`"paymentHoldExpiresAt":"2026-05-21T21:44:59.047689+03:00"`,
		`"paymentHoldExpiresAt":null`,
		`"paymentDecidedBy":"2026-05-21T22:05:00+03:00"`,
		`"paymentDecidedBy":null`),
	orderProcessing: strings.NewReplacer(
		`"paymentStatus":"PENDING"`, `"paymentStatus":"PROCESSING"`,
		"2026-05-21T21:44:59.047689+03:00", payableHold.Format(time.RFC3339),
		`"paymentDecidedBy":"2026-05-21T22:05:00+03:00"`,
		`"paymentDecidedBy":null`),
	orderCanceledPaid: strings.NewReplacer(
		`"status":"PENDING","statusDisplay"`,
		`"status":"CANCELED","statusDisplay"`,
		`"paymentStatus":"PENDING"`, `"paymentStatus":"COMPLETED"`,
		`"paymentHoldExpiresAt":"2026-05-21T21:44:59.047689+03:00"`,
		`"paymentHoldExpiresAt":null`,
		`"paymentDecidedBy":"2026-05-21T22:05:00+03:00"`,
		`"paymentDecidedBy":null`),
}

func TestBuildCheckoutMatchesCheckoutSchema(t *testing.T) {
	schema := checkoutSchema(t)
	tn := testTenant()
	b := NewBuilder(
		fixtureDjango(t),
		"https://{assets_host}/img/{path}.webp",
		"assets.platform.test",
		"production",
		storefront.NewSite("el"),
	)

	newSession := func(status checkout.Status) *checkout.Session {
		s := checkout.NewSession("public", tn.Domain, "ucp",
			"29eb4495-e018-45e7-b59c-6646302bd4ef")
		s.Status = status
		return s
	}

	t.Run("incomplete session", func(t *testing.T) {
		s := newSession(checkout.StatusIncomplete)
		payload, err := b.BuildCheckout(t.Context(), tn, s, checkoutCaps)
		require.NoError(t, err)
		require.NoError(t, schema.Validate(roundTrip(t, payload)))

		// Fixture cart: one line, 2 × 464.68 EUR = 929.36 → minor units.
		require.Len(t, payload.LineItems, 1)
		assert.EqualValues(t, 46468, payload.LineItems[0].Item.Price)
		assert.EqualValues(t, 92936, payload.Totals[0].Amount)
		assert.NotEmpty(t, payload.Messages, "missing-input info messages")

		// Legal links must land on pages the storefront serves; the
		// terms link used to point at a route that does not exist.
		links := map[string]string{}
		for _, l := range payload.Links {
			links[l.Type] = l.URL
		}
		assert.Equal(t, "https://shop.example.test/terms-of-use",
			links["terms_of_service"])
		assert.Equal(t, "https://shop.example.test/privacy-policy",
			links["privacy_policy"])
	})

	placed := func(orderUUID string) *checkout.Session {
		s := newSession(checkout.StatusRequiresEscalation)
		s.OrderID, s.OrderUUID = 684, orderUUID
		s.PlacedAt = time.Now().UTC().Add(-time.Minute)
		s.ExpiresAt = s.PlacedAt.Add(30 * time.Minute)
		return s
	}
	// What a checkout awaiting its payment tells the platform: one
	// error message whose severity asks for the buyer, as the spec
	// requires of requires_escalation, and where to send them.
	escalation := func(t *testing.T, s *checkout.Session) (*Checkout, Message) {
		t.Helper()
		payload, err := b.BuildCheckout(t.Context(), tn, s, checkoutCaps)
		require.NoError(t, err)
		require.NoError(t, schema.Validate(roundTrip(t, payload)))
		assert.Equal(t, "requires_escalation", payload.Status)
		require.NotEmpty(t, payload.ContinueURL)
		require.Len(t, payload.Messages, 1)
		return payload, payload.Messages[0]
	}

	t.Run("a payable order sends the buyer to its payment page",
		func(t *testing.T) {
			s := placed(orderPayable)
			s.PaymentURL = "https://www.vivapayments.com/web/checkout?ref=42"
			payload, m := escalation(t, s)

			assert.Equal(t, s.PaymentURL, payload.ContinueURL)
			assert.Equal(t, "error", m.Type)
			assert.Equal(t, "payment_required", m.Code)
			assert.Equal(t, "requires_buyer_input", m.Severity)
			assert.Contains(t, m.Text, payableHold.Format(time.RFC3339))
			// It expires when Django will have decided the order.
			assert.Equal(t, payableDecided.Format(time.RFC3339),
				payload.ExpiresAt)
			assert.Equal(t, payableDecided, s.ExpiresAt)
		})

	t.Run("a payable order without a link hands off to the order",
		func(t *testing.T) {
			s := placed(orderPayable)
			payload, m := escalation(t, s)

			assert.Contains(t, payload.ContinueURL,
				"/checkout/success/"+s.OrderUUID)
			assert.Equal(t, "payment_link_unavailable", m.Code)
			assert.Equal(t, "recoverable", m.Severity)
		})

	t.Run("an order whose time to pay ran out mints no link",
		func(t *testing.T) {
			s := placed(orderHoldPassed)
			s.PaymentURL = "https://www.vivapayments.com/web/checkout?ref=42"
			payload, m := escalation(t, s)

			assert.Contains(t, payload.ContinueURL,
				"/checkout/success/"+s.OrderUUID)
			assert.Equal(t, "error", m.Type)
			assert.Equal(t, "payment_link_expired", m.Code)
			assert.Equal(t, "requires_buyer_review", m.Severity)
			assert.Contains(t, m.Text, "nothing is charged")
			// Its recorded decision is long past: the expiry the
			// checkout already published stands.
			assert.Equal(t, s.PlacedAt.Add(30*time.Minute).Format(
				time.RFC3339), payload.ExpiresAt)
		})

	// Django keeps a failed attempt's link payable: the buyer may try
	// again on the same page until its hold ends.
	t.Run("a failed attempt can be tried again", func(t *testing.T) {
		s := placed(orderPaymentFailed)
		s.PaymentURL = "https://www.vivapayments.com/web/checkout?ref=42"
		payload, m := escalation(t, s)

		assert.Equal(t, s.PaymentURL, payload.ContinueURL)
		assert.Equal(t, "payment_failed", m.Code)
		assert.Equal(t, "requires_buyer_input", m.Severity)
		assert.Contains(t, m.Text, payableHold.Format(time.RFC3339))
		assert.Equal(t, payableDecided.Format(time.RFC3339),
			payload.ExpiresAt)
	})

	// A payment being processed keeps its hold, and nothing decides it:
	// the buyer is not sent to pay a second time.
	t.Run("a payment being processed is not paid again",
		func(t *testing.T) {
			s := placed(orderProcessing)
			s.PaymentURL = "https://www.vivapayments.com/web/checkout?ref=42"
			payload, m := escalation(t, s)

			assert.Contains(t, payload.ContinueURL,
				"/checkout/success/"+s.OrderUUID)
			assert.Equal(t, "payment_processing", m.Code)
			assert.Equal(t, "requires_buyer_review", m.Severity)
			assert.Equal(t, s.PlacedAt.Add(24*time.Hour).Format(
				time.RFC3339), payload.ExpiresAt)
		})

	// Nothing will decide an order whose charge did not match its total,
	// or whose store's sweep is off, and Django does not say which: the
	// buyer is neither sent to pay again nor told nothing is charged.
	t.Run("an order nothing will decide is not paid again",
		func(t *testing.T) {
			// The recorded order: its payment pending, no decision.
			s := placed("b9be45e5-6062-4976-ae7b-2c31eb2ad689")
			s.PaymentURL = "https://www.vivapayments.com/web/checkout?ref=42"
			payload, m := escalation(t, s)

			assert.Contains(t, payload.ContinueURL,
				"/checkout/success/"+s.OrderUUID)
			assert.Equal(t, "payment_in_review", m.Code)
			assert.Equal(t, "requires_buyer_review", m.Severity)
			assert.NotContains(t, m.Text, "nothing is charged")
			assert.Equal(t, s.PlacedAt.Add(24*time.Hour).Format(
				time.RFC3339), payload.ExpiresAt)
		})

	t.Run("a payment after the cancel leaves the checkout canceled",
		func(t *testing.T) {
			s := placed(orderCanceledPaid)
			payload, err := b.BuildCheckout(t.Context(), tn, s,
				checkoutCaps)
			require.NoError(t, err)
			require.NoError(t, schema.Validate(roundTrip(t, payload)))

			assert.Equal(t, "canceled", payload.Status)
			assert.Equal(t, checkout.StatusCanceled, s.Status)
			assert.Empty(t, payload.ContinueURL)
			require.Len(t, payload.Messages, 1)
			assert.Equal(t, "info", payload.Messages[0].Type)
			assert.Contains(t, payload.Messages[0].Text, "refunds")
			assert.Equal(t, s.PlacedAt.Add(24*time.Hour).Format(
				time.RFC3339), payload.ExpiresAt)
		})

	// Django settles a payment as canceled with its order whatever it
	// was, so the checkout never says nothing was charged.
	t.Run("a canceled unpaid order owes nothing", func(t *testing.T) {
		s := placed(orderCanceledUnpaid)
		payload, err := b.BuildCheckout(t.Context(), tn, s, checkoutCaps)
		require.NoError(t, err)
		require.NoError(t, schema.Validate(roundTrip(t, payload)))
		assert.Equal(t, "canceled", payload.Status)
		require.Len(t, payload.Messages, 1)
		assert.Contains(t, payload.Messages[0].Text, "owes nothing")
		assert.NotContains(t, payload.Messages[0].Text, "nothing was charged")
	})

	t.Run("an open checkout expires with its session", func(t *testing.T) {
		s := newSession(checkout.StatusIncomplete)
		payload, err := b.BuildCheckout(t.Context(), tn, s, checkoutCaps)
		require.NoError(t, err)
		assert.Equal(t, s.ExpiresAt.Format(time.RFC3339), payload.ExpiresAt)
		assert.WithinDuration(t, time.Now().Add(30*time.Minute),
			s.ExpiresAt, time.Minute)
	})

	t.Run("capabilities_incompatible is a valid error_response",
		func(t *testing.T) {
			errSchema := compileUCP(t, "common/types/error_response.json")
			require.NoError(t, errSchema.Validate(roundTrip(t,
				CapabilitiesIncompatible("https://shop.example.test/cart"))))
		})

	t.Run("a checkout not found is a valid error_response",
		func(t *testing.T) {
			errSchema := compileUCP(t, "common/types/error_response.json")
			require.NoError(t, errSchema.Validate(roundTrip(t,
				CheckoutNotFound(checkoutCaps, "no checkout"))))
		})

	t.Run("coupon cart emits a negative discount totals line",
		func(t *testing.T) {
			couponBuilder := NewBuilder(
				fixtureDjango(t, "cart_with_coupon.json"),
				"https://{assets_host}/img/{path}.webp",
				"assets.platform.test",
				"production",
				storefront.NewSite("el"),
			)
			s := newSession(checkout.StatusIncomplete)
			payload, err := couponBuilder.BuildCheckout(
				t.Context(), tn, s, checkoutCaps)
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
		payload, err := b.BuildCheckout(t.Context(), tn, s, checkoutCaps)
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
	schema := checkoutSchema(t)
	dj := fixtureDjango(t)

	build := func(t *testing.T, tn *tenant.Tenant, st checkout.Status,
		paymentURL string,
	) *Checkout {
		t.Helper()
		b := NewBuilder(dj, "https://{assets_host}/img/{path}.webp",
			"assets.platform.test", "production", storefront.NewSite("el"))
		s := checkout.NewSession(tn.SchemaName, tn.Domain, "ucp",
			"29eb4495-e018-45e7-b59c-6646302bd4ef")
		s.Status = st
		s.PaymentURL = paymentURL
		if st == checkout.StatusRequiresEscalation {
			s.OrderID, s.OrderUUID = 684, orderPayable
			s.PlacedAt = time.Now().UTC()
		}
		payload, err := b.BuildCheckout(t.Context(), tn, s, checkoutCaps)
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
				"an open checkout hands out no link to its cart")
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

	out, err := BuildOrder(tn, storefront.NewSite("el").Links(tn.Domain, "el"), &order, "chk_abc123", OrderCapabilities())
	require.NoError(t, err)
	require.NoError(t, schema.Validate(roundTrip(t, out)))

	assert.Equal(t, "chk_abc123", out.CheckoutID)
	assert.Equal(t, order.UUID, out.ID)
	assert.Contains(t, out.PermalinkURL, "/checkout/success/"+order.UUID)
	require.NotEmpty(t, out.LineItems)

	// The product was renamed after the sale: the line keeps the name it
	// was sold under, and the live product only for its id.
	assert.Equal(t, "Ιταλικά Ζυμαρικά Classic", out.LineItems[0].Item.Title)
	assert.Equal(t, "369773", out.LineItems[0].Item.ID)

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
	_, err := BuildOrder(testTenant(), storefront.Links{}, &django.Order{UUID: "o-1"}, "",
		OrderCapabilities())
	assert.ErrorContains(t, err, "no known checkout session")
}

func TestOrderLineStatusFollowsTheSchemaDefinition(t *testing.T) {
	assert.Equal(t, "removed", orderLineStatus(0, 0))
	assert.Equal(t, "fulfilled", orderLineStatus(3, 3))
	assert.Equal(t, "partial", orderLineStatus(3, 1))
	assert.Equal(t, "processing", orderLineStatus(3, 0))
}
