package checkout

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// A render records what an open checkout shows, its total and the digest
// of the cart that total is of, and says whether it is another than what
// is stored, so the surface stores it only then: the same total of
// another cart is another. A placed checkout shows its order's, which
// nothing is placed at.
func TestShowRecordsWhatAnOpenCheckoutShows(t *testing.T) {
	s := NewSession("public", "shop.example.test", ProtocolUCP, "cart")
	p := &Pricing{Total: 18550, Currency: "EUR", CartDigest: "a"}

	assert.True(t, s.Show(p), "the first total shown")
	assert.Equal(t, &Shown{
		Total:      django.ExpectedTotal{Amount: 18550, Currency: "EUR"},
		CartDigest: "a",
	}, s.Shown)
	assert.False(t, s.Show(p), "the same is not stored again")
	p.Total = 18600
	assert.True(t, s.Show(p), "a new total is")
	assert.EqualValues(t, 18600, s.Shown.Total.Amount)
	p.CartDigest = "b"
	assert.True(t, s.Show(p), "and the same total of another cart")
	assert.Equal(t, "b", s.Shown.CartDigest)

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

// An order is placed only at what was shown of an order: a total the
// quote priced with its cart's digest. A total shown before a delivery
// and a pay way were both priced is the rows', and no order's.
func TestOnlyAnOrdersTotalIsOneToPlaceAt(t *testing.T) {
	var none *Shown
	assert.False(t, none.OfAnOrder(), "nothing shown")
	assert.False(t, (&Shown{Total: django.ExpectedTotal{Amount: 100}}).
		OfAnOrder(), "the rows priced so far")
	assert.True(t, (&Shown{CartDigest: "a"}).OfAnOrder())
}
