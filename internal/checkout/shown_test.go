package checkout

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// A render records the total an open checkout shows, and says whether it
// is another than the one stored, so the surface stores it only then. A
// placed checkout shows its order's, which nothing is placed at.
func TestShowRecordsTheTotalAnOpenCheckoutShows(t *testing.T) {
	s := NewSession("public", "shop.example.test", ProtocolUCP, "cart")
	p := &Pricing{Total: 18550, Currency: "EUR"}

	assert.True(t, s.Show(p), "the first total shown")
	assert.Equal(t, &django.ExpectedTotal{Amount: 18550, Currency: "EUR"},
		s.ShownTotal)
	assert.False(t, s.Show(p), "the same total is not stored again")
	p.Total = 18600
	assert.True(t, s.Show(p), "a new total is")
	assert.EqualValues(t, 18600, s.ShownTotal.Amount)

	for _, status := range []Status{StatusCompleteInProgress,
		StatusRequiresEscalation, StatusCompleted, StatusCanceled} {
		s.Status = status
		p.Total++
		assert.False(t, s.Show(p), status)
	}
	s.Status = StatusReadyForComplete
	assert.False(t, s.Show(&Pricing{Total: 1, Order: &django.Order{}}),
		"an order's totals are its own")
}
