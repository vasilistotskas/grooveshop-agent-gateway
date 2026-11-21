package acp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// line_one is the one street line, which Django splits, and line_two
// what it does not hold: both are kept as sent and rendered back.
func TestAddressLinesAreKeptAsSent(t *testing.T) {
	s := checkout.NewSession("public", testTenant().Domain, "acp",
		"29eb4495-e018-45e7-b59c-6646302bd4ef")
	applyFulfillment(s, &FulfillmentDetails{Address: &Address{
		LineOne: " Πανεπιστημίου 12 ", LineTwo: "3ος όροφος",
		City: "Αθήνα", PostalCode: "10431", Country: "GR",
	}}, []SelectedFulfillmentOption{{OptionID: "acs:home_delivery"}})

	assert.Equal(t, "Πανεπιστημίου 12", s.Fulfillment.Street)
	assert.Equal(t, "3ος όροφος", s.Fulfillment.AddressLine2)
	s.Buyer = checkout.Buyer{
		FirstName: "Μαρία", LastName: "Παπαδοπούλου",
		Email: "maria@example.test", Phone: "+306912345678",
	}
	s.Recompute()
	assert.Equal(t, checkout.StatusReadyForComplete, s.Status,
		"a line without a number is Django's to refuse, not a guess here")

	payload, err := Render(t.Context(), fixtureDjango(t),
		testTenant(), testLinks(testTenant()), s)
	require.NoError(t, err)
	require.NotNil(t, payload.FulfillmentDetails)
	assert.Equal(t, "Πανεπιστημίου 12",
		payload.FulfillmentDetails.Address.LineOne)
	assert.Equal(t, "3ος όροφος", payload.FulfillmentDetails.Address.LineTwo)
}

// A street line Django refused at completion keeps the session from
// being ready, and its read says which field to fix, until the address
// changes.
func TestARefusedStreetLineIsNamedUntilItChanges(t *testing.T) {
	s := checkout.NewSession("public", testTenant().Domain, "acp",
		"29eb4495-e018-45e7-b59c-6646302bd4ef")
	address := &FulfillmentDetails{Address: &Address{
		LineOne: "Πανεπιστημίου", City: "Αθήνα",
		PostalCode: "10431", Country: "GR",
	}}
	applyFulfillment(s, address,
		[]SelectedFulfillmentOption{{OptionID: "acs:home_delivery"}})
	s.Buyer = checkout.Buyer{
		FirstName: "Μαρία", LastName: "Παπαδοπούλου",
		Email: "maria@example.test", Phone: "+306912345678",
	}
	s.DestinationRefused = &checkout.Refusal{
		Code: django.CodeStreetNumberMissing, Detail: "Add the number."}
	s.Recompute()
	require.Equal(t, checkout.StatusIncomplete, s.Status)

	payload, err := Render(t.Context(), fixtureDjango(t),
		testTenant(), testLinks(testTenant()), s)
	require.NoError(t, err)
	var params []string
	for _, m := range payload.Messages {
		if m.Type == "error" {
			params = append(params, m.Code+" "+m.Param)
		}
	}
	assert.Equal(t,
		[]string{"invalid $.fulfillment_details.address.line_one"}, params)

	applyFulfillment(s, address, nil)
	assert.NotNil(t, s.DestinationRefused, "the same address")
	address.Address.LineOne = "Πανεπιστημίου 12"
	applyFulfillment(s, address, nil)
	assert.Nil(t, s.DestinationRefused)
}
