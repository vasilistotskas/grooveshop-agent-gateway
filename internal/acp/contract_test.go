package acp

import (
	"context"
	"encoding/json"
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

func compileACP(t *testing.T, def string) *jsonschema.Schema {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "schemas", "acp",
		"2026-04-17", "schema.agentic_checkout.json")
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	raw, err := jsonschema.UnmarshalJSON(f)
	require.NoError(t, err)

	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource("acp-checkout.json", raw))
	schema, err := c.Compile("acp-checkout.json#/$defs/" + def)
	require.NoError(t, err)
	return schema
}

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

func TestRenderMatchesCheckoutSessionSchema(t *testing.T) {
	sessionSchema := compileACP(t, "CheckoutSession")
	withOrderSchema := compileACP(t, "CheckoutSessionWithOrder")
	tn := testTenant()
	dj := fixtureDjango(t)

	newSession := func() *checkout.Session {
		return checkout.NewSession("public", tn.Domain, "acp",
			"29eb4495-e018-45e7-b59c-6646302bd4ef")
	}

	t.Run("fresh session is not_ready_for_payment", func(t *testing.T) {
		s := newSession()
		s.Recompute()
		payload, err := Render(context.Background(), dj, tn, s)
		require.NoError(t, err)
		require.NoError(t, sessionSchema.Validate(roundTrip(t, payload)))

		assert.Equal(t, "not_ready_for_payment", payload.Status)
		assert.NotEmpty(t, payload.Messages)
		assert.Contains(t, payload.ContinueURL, "/cart/claim?uuid=")
		require.Len(t, payload.LineItems, 1)
		assert.EqualValues(t, 46468, payload.LineItems[0].Item.UnitAmount)

		// Legal links must land on pages the storefront serves; the
		// terms link used to point at a route that does not exist.
		links := map[string]string{}
		for _, l := range payload.Links {
			links[l.Type] = l.URL
		}
		assert.Equal(t, "https://shop.example.test/terms-of-use",
			links["terms_of_use"])
		assert.Equal(t, "https://shop.example.test/privacy-policy",
			links["privacy_policy"])
	})

	t.Run("filled session is ready_for_payment with COD available",
		func(t *testing.T) {
			s := newSession()
			s.Buyer = checkout.Buyer{
				FirstName: "Μαρία", LastName: "Παπαδοπούλου",
				Email: "maria@example.test", Phone: "+306912345678",
			}
			s.Fulfillment = checkout.Fulfillment{
				Kind:         checkout.FulfillmentHomeDelivery,
				ProviderCode: "acs",
				CountryCode:  "GR", City: "Αθήνα", Zipcode: "10431",
				Street: "Πανεπιστημίου", StreetNumber: "12",
			}
			// No pay way selected: on ACP that is the platform's concern,
			// so readiness derives from buyer + fulfillment alone.
			s.Recompute()
			require.Equal(t, checkout.StatusReadyForComplete, s.Status)

			payload, err := Render(context.Background(), dj, tn, s)
			require.NoError(t, err)
			require.NoError(t, sessionSchema.Validate(roundTrip(t, payload)))

			assert.Equal(t, "ready_for_payment", payload.Status)
			require.NotNil(t, payload.FulfillmentDetails)
			assert.Equal(t, "GR", payload.FulfillmentDetails.Address.Country)
			require.NotEmpty(t, payload.SelectedFulfillmentOptions)
			assert.Equal(t, "acs:home_delivery",
				payload.SelectedFulfillmentOptions[0].OptionID)
		})

	t.Run("completed session validates as CheckoutSessionWithOrder",
		func(t *testing.T) {
			s := newSession()
			s.Status = checkout.StatusCompleted
			s.OrderID = 684
			s.OrderUUID = "b9be45e5-6062-4976-ae7b-2c31eb2ad689"
			payload, err := Render(context.Background(), dj, tn, s)
			require.NoError(t, err)
			require.NoError(t,
				withOrderSchema.Validate(roundTrip(t, payload)))
			require.NotNil(t, payload.Order)
			assert.Equal(t, s.ID, payload.Order.CheckoutSessionID)
		})

	t.Run("every response declares the discount extension", func(t *testing.T) {
		s := newSession()
		s.Recompute()
		payload, err := Render(context.Background(), dj, tn, s)
		require.NoError(t, err)
		require.Len(t, payload.Capabilities.Extensions, 1)
		ext := payload.Capabilities.Extensions[0]
		assert.Equal(t, "discount", ext.Name)
		assert.Contains(t, ext.Extends, "$.CheckoutSession.discounts")
		require.NoError(t, sessionSchema.Validate(roundTrip(t, payload)))
	})

	t.Run("applied coupon renders discounts and totals", func(t *testing.T) {
		couponDj := fixtureDjango(t, "cart_with_coupon.json")
		s := newSession()
		s.DiscountCodes = []string{"SAVE10"}
		s.Recompute()

		payload, err := Render(context.Background(), couponDj, tn, s)
		require.NoError(t, err)
		require.NoError(t, sessionSchema.Validate(roundTrip(t, payload)))

		require.NotNil(t, payload.Discounts)
		assert.Equal(t, []string{"SAVE10"}, payload.Discounts.Codes)
		require.Len(t, payload.Discounts.Applied, 1)
		applied := payload.Discounts.Applied[0]
		assert.Equal(t, "SAVE10", applied.Code)
		assert.Equal(t, "SAVE10", applied.Coupon.ID)
		assert.EqualValues(t, 9294, applied.Amount)
		assert.False(t, applied.Automatic)
		assert.Empty(t, payload.Discounts.Rejected)

		// Totals: 92936 subtotal, positive 9294 discount row, total
		// already reduced.
		byType := map[string]int64{}
		for _, total := range payload.Totals {
			byType[total.Type] = total.Amount
		}
		assert.EqualValues(t, 9294, byType["discount"])
		assert.EqualValues(t, 83642, byType["total"])
	})

	t.Run("rejected code renders discounts.rejected and a warning",
		func(t *testing.T) {
			s := newSession()
			s.DiscountCodes = []string{"DEAD"}
			s.RejectedDiscounts = []checkout.DiscountRejection{{
				Code:    "DEAD",
				Reason:  "discount_code_expired",
				Message: "The promotion has ended",
			}}
			s.Recompute()

			payload, err := Render(context.Background(), dj, tn, s)
			require.NoError(t, err)
			require.NoError(t, sessionSchema.Validate(roundTrip(t, payload)))

			require.NotNil(t, payload.Discounts)
			assert.Empty(t, payload.Discounts.Applied)
			require.Len(t, payload.Discounts.Rejected, 1)
			rej := payload.Discounts.Rejected[0]
			assert.Equal(t, "DEAD", rej.Code)
			assert.Equal(t, "discount_code_expired", rej.Reason)
			assert.Equal(t, "The promotion has ended", rej.Message)

			var warning *Message
			for i := range payload.Messages {
				if payload.Messages[i].Type == "warning" {
					warning = &payload.Messages[i]
				}
			}
			require.NotNil(t, warning, "rejections surface as warnings")
			assert.Equal(t, "discount_code_expired", warning.Code)
			assert.Equal(t, "$.discounts.codes", warning.Param)

			// No discount totals row without an applied discount.
			for _, total := range payload.Totals {
				assert.NotEqual(t, "discount", total.Type)
			}
		})

	t.Run("canceled session keeps the shared status name", func(t *testing.T) {
		s := newSession()
		s.Status = checkout.StatusCanceled
		payload, err := Render(context.Background(), dj, tn, s)
		require.NoError(t, err)
		require.NoError(t, sessionSchema.Validate(roundTrip(t, payload)))
		assert.Equal(t, "canceled", payload.Status)
	})
}

func TestSplitStreetNumber(t *testing.T) {
	cases := []struct{ in, street, number string }{
		{"Ερμού 12", "Ερμού", "12"},
		{"Ερμού 12Β", "Ερμού", "12Β"},
		{"Λεωφ. Κηφισίας 128", "Λεωφ. Κηφισίας", "128"},
		{"Ερμού", "Ερμού", ""},
		{"  Ερμού 5  ", "Ερμού", "5"},
		{"", "", ""},
	}
	for _, c := range cases {
		street, number := splitStreetNumber(c.in)
		if street != c.street || number != c.number {
			t.Errorf("splitStreetNumber(%q) = %q,%q want %q,%q",
				c.in, street, number, c.street, c.number)
		}
	}
}
