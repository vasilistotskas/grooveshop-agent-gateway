package ucp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// A choice the store refuses is a recoverable error at its member, with
// the store's own words and what to do; none refused, none listed.
func TestRefusedChoicesAreRecoverableErrorsAtTheirMember(t *testing.T) {
	assert.Empty(t, refusedChoices(&checkout.Pricing{}))

	got := refusedChoices(&checkout.Pricing{
		DeliveryRefused: &checkout.Refusal{
			Code: "shipping_weight_exceeded", Detail: "Too heavy."},
		PayWayRefused: &checkout.Refusal{
			Code: "pay_way_unavailable", Detail: "Not offered."},
	})

	require.Len(t, got, 2)
	assert.Equal(t, "shipping_weight_exceeded", got[0].Code)
	assert.Equal(t, "$.fulfillment", got[0].Path)
	assert.Equal(t, "pay_way_unavailable", got[1].Code)
	assert.Equal(t, "$.payment", got[1].Path)
	for _, m := range got {
		assert.Equal(t, "error", m.Type)
		assert.Equal(t, "recoverable", m.Severity)
		assert.Contains(t, m.Text, "update_checkout")
	}
	assert.Contains(t, got[0].Text, "Too heavy.")
}
