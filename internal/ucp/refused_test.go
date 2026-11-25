package ucp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// A choice the store refuses is a recoverable error at its member, with
// the store's own words and what to do; none refused, none listed.
func TestRefusedChoicesAreRecoverableErrorsAtTheirMember(t *testing.T) {
	assert.Empty(t, refusedChoices(&checkout.Session{}, &checkout.Pricing{}))

	got := refusedChoices(&checkout.Session{}, &checkout.Pricing{
		DeliveryRefused: &checkout.Refusal{
			Code: "shipping_weight_exceeded", Detail: "Too heavy."},
		PayWayRefused: &checkout.Refusal{
			Code: "pay_way_unavailable", Detail: "Not offered."},
	})

	require.Len(t, got, 2)
	assert.Equal(t, "shipping_weight_exceeded", got[0].Code)
	assert.Equal(t, pathOption, got[0].Path)
	assert.Equal(t, "pay_way_unavailable", got[1].Code)
	assert.Equal(t, "$.payment", got[1].Path)
	for _, m := range got {
		assert.Equal(t, "error", m.Type)
		assert.Equal(t, "recoverable", m.Severity)
		assert.Contains(t, m.Text, "update_checkout")
	}
	assert.Contains(t, got[0].Text, "Too heavy.")
}

// A delivery no carrier makes to the region is refused at the region: with
// no option left, another option is no remedy. While a carrier still
// delivers there, it is.
func TestARegionNoCarrierDeliversToIsAtTheRegion(t *testing.T) {
	refused := &checkout.Refusal{
		Code: django.CodeShippingRegionUnavailable, Detail: "Not there."}
	for name, tc := range map[string]struct {
		options []django.QuoteShippingOption
		path    string
		remedy  string
	}{
		"none left": {
			[]django.QuoteShippingOption{{Kind: "pickup_point"}},
			pathRegion, "another region",
		},
		"another carrier": {
			[]django.QuoteShippingOption{{Kind: "home_delivery"}},
			pathOption, "another of the group's options",
		},
		"another carrier the cart does not fit": {
			[]django.QuoteShippingOption{
				{Kind: "home_delivery", ExceedsMaxWeight: true}},
			pathOption, "another of the group's options",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := refusedChoices(
				&checkout.Session{Fulfillment: checkout.Fulfillment{
					CountryCode: "GR", RegionID: "GR-M"}},
				&checkout.Pricing{
					DeliveryRefused: refused, ShippingOptions: tc.options})
			require.Len(t, got, 1)
			assert.Equal(t, tc.path, got[0].Path)
			assert.Equal(t, django.CodeShippingRegionUnavailable, got[0].Code)
			assert.Contains(t, got[0].Text, "Not there.")
			assert.Contains(t, got[0].Text, tc.remedy)
		})
	}
}

// A refused pickup is at the point: its one option is the point's
// network, so the remedy is another point or the shipping method.
func TestARefusedPickupIsAtThePoint(t *testing.T) {
	s := &checkout.Session{Fulfillment: checkout.Fulfillment{
		Kind: checkout.FulfillmentPickupPoint, ProviderCode: "boxnow",
		CountryCode: "GR", BoxnowLockerID: "25",
	}}
	got := refusedChoices(s, &checkout.Pricing{
		DeliveryRefused: &checkout.Refusal{
			Code: "shipping_parcel_too_large", Detail: "Too large."},
	})

	require.Len(t, got, 1)
	assert.Equal(t, pathPoint, got[0].Path)
	assert.Contains(t, got[0].Text, "another pickup point")
	assert.Contains(t, got[0].Text, "Too large.")
}
