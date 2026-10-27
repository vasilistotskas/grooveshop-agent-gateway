package acp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// Django refuses an order without a street number, so a line_one with no
// house number must keep the session from reporting itself ready, and say
// which field to fix.
func TestRenderAsksForTheHouseNumber(t *testing.T) {
	s := checkout.NewSession("public", testTenant().Domain, "acp",
		"29eb4495-e018-45e7-b59c-6646302bd4ef")
	applyFulfillment(s, &FulfillmentDetails{Address: &Address{
		LineOne: "Πανεπιστημίου", City: "Αθήνα",
		PostalCode: "10431", Country: "GR",
	}}, []SelectedFulfillmentOption{{OptionID: "acs:home_delivery"}})
	s.Buyer = checkout.Buyer{
		FirstName: "Μαρία", LastName: "Παπαδοπούλου",
		Email: "maria@example.test", Phone: "+306912345678",
	}
	s.Recompute()
	require.Equal(t, checkout.StatusIncomplete, s.Status)

	payload, err := Render(context.Background(), fixtureDjango(t),
		testTenant(), testLinks(testTenant()), s)
	require.NoError(t, err)

	var params []string
	for _, m := range payload.Messages {
		if m.Code == "missing" {
			params = append(params, m.Param)
		}
	}
	assert.Equal(t,
		[]string{"$.fulfillment_details.address.line_one"}, params)
}
