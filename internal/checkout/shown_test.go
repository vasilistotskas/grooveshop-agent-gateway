package checkout

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// shownBy is what Show stores from a render of p.
func shownBy(p *Pricing) *Shown {
	s := readySession()
	s.Show(p)
	return s.Shown
}

// A completion names a choice the store refuses now before it asks for
// a total: a checkout that showed a refused delivery showed no order's
// total either, and only the refusal says what to change. One that
// showed nothing, or only the rows of a checkout not yet priced as an
// order, is asked to be read first.
func TestAdmitNamesARefusedChoiceBeforeTheShownTotal(t *testing.T) {
	refusing := func(r django.QuoteRequest) (int, string) {
		if r.ShippingKind != "" {
			return http.StatusBadRequest, `{"detail":"Too heavy.",` +
				`"reason":"shipping_weight_exceeded",` +
				`"code":"shipping_weight_exceeded"}`
		}
		return http.StatusOK, quoteBody("null",
			`{"amount":2.00,"waived":false}`, "null")
	}
	pricing := func(django.QuoteRequest) (int, string) {
		return http.StatusOK, quoteBody(`{"price":3.50,"waivedBy":null}`,
			`{"amount":2.00,"waived":false}`,
			`{"amount":18550,"currency":"EUR"}`)
	}
	ready := func() *Session {
		s := readySession()
		s.Buyer = completeBuyer()
		s.Fulfillment.City, s.Fulfillment.Zipcode = "Αθήνα", "10431"
		s.Fulfillment.Street, s.Fulfillment.StreetNumber = "Ερμού", "12"
		s.Recompute()
		return s
	}
	rows := &Shown{Total: django.ExpectedTotal{Amount: 18000, Currency: "EUR"}}
	for name, tc := range map[string]struct {
		quote func(django.QuoteRequest) (int, string)
		shown *Shown
		want  error
	}{
		"a refused delivery, its rows shown": {refusing, rows, ErrChoiceRefused},
		"a refused delivery, nothing shown":  {refusing, nil, ErrChoiceRefused},
		"nothing shown":                      {pricing, nil, ErrTotalNotShown},
		"only the rows shown":                {pricing, rows, ErrTotalNotShown},
		// The last read named its pay way refused: the store takes it
		// again now, but the checkout showed it refused, not an order.
		"a render that named a refused pay way": {pricing,
			shownBy(&Pricing{Total: 18550, Currency: "EUR",
				CartDigest: "digest-of-the-cart",
				PayWayRefused: &Refusal{
					Code: "pay_way_unavailable", Detail: "Not now.",
				}}), ErrTotalNotShown},
	} {
		t.Run(name, func(t *testing.T) {
			dj, _ := quoteDjango(t, pricingCart("20.00", false), tc.quote)
			s := ready()
			require.Equal(t, StatusReadyForComplete, s.Status)
			s.Shown = tc.shown
			err := (&Flow{dj: dj}).admit(t.Context(), pricingTenant(), s)
			require.ErrorIs(t, err, tc.want)
			require.ErrorIs(t, err, ErrNotReady)
		})
	}
}
