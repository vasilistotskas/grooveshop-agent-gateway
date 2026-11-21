package ucp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
)

// A pickup location's id is opaque to a platform and read back exactly,
// whatever its parts hold; one this business could not have given is
// refused.
func TestPickupLocationIDsReadBack(t *testing.T) {
	for _, l := range []PickupLocation{
		{Provider: "boxnow", Country: "GR", ID: "25"},
		{Provider: "acs", Country: "GR", ID: "AAT", Branch: "502"},
		{Provider: "acs", Country: "CY", ID: "a:b/c", Branch: "1 2"},
	} {
		id := l.DestinationID()
		got, err := ParsePickupLocation(id)
		require.NoError(t, err, id)
		assert.Equal(t, l, got, id)
	}
	assert.Equal(t, "acs:GR:AAT:502", PickupLocation{
		Provider: "acs", Country: "GR", ID: "AAT", Branch: "502",
	}.DestinationID())
	for _, id := range []string{"", "shipping_address", "boxnow:GR",
		"boxnow:GR:25:9", "acs:GR:AAT", "elta:GR:1", "boxnow::25",
		"boxnow:GR:%zz"} {
		_, err := ParsePickupLocation(id)
		require.ErrorIs(t, err, ErrUnknownLocation, id)
	}
}

// The pickup method, asserted member by member: listed where the store
// delivers the cart to pickup points, the ACS points near the buyer's
// locality its destinations, the one selected among them by its id with
// its name, and the selected point's network the one option of its
// group. The cart's lines are the chosen method's.
func TestBuildCheckoutRendersThePickupMethod(t *testing.T) {
	schema := checkoutSchema(t)
	tn := testTenant()
	b := NewBuilder(fixtureDjango(t), "https://{assets_host}/img/{path}.webp",
		"assets.platform.test", "production", storefront.NewSite("el"))
	s := checkout.NewSession("public", tn.Domain, "ucp",
		"29eb4495-e018-45e7-b59c-6646302bd4ef")
	s.Locality = checkout.Locality{CountryCode: "GR", PostalCode: "10434"}
	s.Deliver(checkout.Fulfillment{
		Kind: checkout.FulfillmentPickupPoint, ProviderCode: "acs",
		CountryCode: "GR", AcsStationExternalID: "GGL",
		AcsStationBranch: "519",
	})

	payload, err := b.BuildCheckout(t.Context(), tn, s, checkoutCaps)
	require.NoError(t, err)
	require.NoError(t, schema.Validate(roundTrip(t, payload)))
	methods := payload.Fulfillment.Methods
	require.Len(t, methods, 2)
	assert.Equal(t, "shipping", methods[0].ID)
	assert.Empty(t, methods[0].LineItemIDs, "the cart goes by pickup")
	assert.Empty(t, methods[0].Destinations)

	pickup := methods[1]
	assert.Equal(t, "pickup", pickup.ID)
	assert.Equal(t, "pickup", pickup.Type)
	assert.Equal(t, []string{"410"}, pickup.LineItemIDs)
	assert.Equal(t, "acs:GR:GGL:519", pickup.SelectedDestinationID)
	require.Len(t, pickup.Destinations, maxNearPoints, "the nearest few")
	assert.Equal(t, Destination{
		Type: "business_location", ID: "acs:GR:AAT:502",
		Name: "ACS SMARTPOINT LOCKER ΠΕΔΙΟ ΑΡΕΩΣ",
		Address: &PostalAddress{
			StreetAddress:   "PolisPark parking Πλατεία Αιγύπτου",
			AddressLocality: "Πεδίο Άρεως", PostalCode: "10434",
			AddressCountry: "GR",
		},
	}, pickup.Destinations[0])
	assert.Equal(t, "acs:GR:GGL:519", pickup.Destinations[1].ID,
		"the selected point is listed once")
	acs := "acs"
	assert.Equal(t, []FulfillmentGroup{{
		ID: "parcel", LineItemIDs: []string{"410"},
		Options: []FulfillmentOption{{
			ID: "acs", Title: "ACS Courier", Carrier: "ACS Courier",
			Totals: []LineItemTotal{{Type: "total", Amount: 0}},
		}},
		SelectedOptionID: &acs,
	}}, pickup.Groups)

	// A point not near the locality, and one the catalogue did not hold
	// when it was chosen: it leads the list, named by its carrier and id.
	s.Deliver(checkout.Fulfillment{
		Kind: checkout.FulfillmentPickupPoint, ProviderCode: "boxnow",
		CountryCode: "GR", BoxnowLockerID: "25",
	})
	payload, err = b.BuildCheckout(t.Context(), tn, s, checkoutCaps)
	require.NoError(t, err)
	require.NoError(t, schema.Validate(roundTrip(t, payload)))
	pickup = payload.Fulfillment.Methods[1]
	assert.Equal(t, Destination{Type: "business_location",
		ID: "boxnow:GR:25", Name: "BOX NOW 25"}, pickup.Destinations[0])
	require.Len(t, pickup.Groups, 1)
	assert.Equal(t, "boxnow", pickup.Groups[0].Options[0].ID)
}

// What a pickup checkout still needs is an error at the pickup method's
// selected_destination_id: no point yet, or one the store refused when
// the order was sent.
func TestMissingAPickupPointIsAnErrorAtTheSelection(t *testing.T) {
	s := checkout.NewSession("public", "shop.example.test", "ucp", "c")
	s.Buyer = checkout.Buyer{FirstName: "Μαρία", LastName: "Π",
		Email: "maria@example.test", Phone: "+306912345678"}
	s.PayWayID = 1
	s.Deliver(checkout.Fulfillment{Kind: checkout.FulfillmentPickupPoint})

	assert.Equal(t, []Message{{Type: "error", Code: "missing",
		Path: pathPoint, Severity: "recoverable",
		Text: missing(s, true)[0].Text}}, missing(s, true))

	s.Deliver(checkout.Fulfillment{
		Kind: checkout.FulfillmentPickupPoint, ProviderCode: "boxnow",
		CountryCode: "GR", BoxnowLockerID: "999",
	})
	s.DestinationRefused = &checkout.Refusal{
		Code: django.CodePickupPointUnknown}
	assert.Equal(t, []Message{{Type: "error",
		Code: django.CodePickupPointUnknown, Path: pathPoint,
		Severity: "recoverable",
		Text:     DestinationRefusedText(django.CodePickupPointUnknown),
	}}, missing(s, true))
}
