package checkout

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func completeBuyer() Buyer {
	return Buyer{
		FirstName: "Μαρία", LastName: "Παπαδοπούλου",
		Email: "maria@example.test", Phone: "+306912345678",
	}
}

func homeDelivery() Fulfillment {
	return Fulfillment{
		Kind: FulfillmentHomeDelivery, ProviderCode: "acs",
		CountryCode: "GR", City: "Αθήνα", Zipcode: "10431",
		Street: "Πανεπιστημίου", StreetNumber: "12",
	}
}

func TestBuyerComplete(t *testing.T) {
	assert.True(t, completeBuyer().Complete())

	for _, tc := range []struct {
		name   string
		mutate func(*Buyer)
	}{
		{"first name", func(b *Buyer) { b.FirstName = "" }},
		{"last name", func(b *Buyer) { b.LastName = "" }},
		{"email", func(b *Buyer) { b.Email = "" }},
		{"phone", func(b *Buyer) { b.Phone = "" }},
	} {
		t.Run("missing "+tc.name, func(t *testing.T) {
			b := completeBuyer()
			tc.mutate(&b)
			assert.False(t, b.Complete())
		})
	}
}

func TestFulfillmentComplete(t *testing.T) {
	t.Run("home delivery with address", func(t *testing.T) {
		assert.True(t, homeDelivery().Complete())
	})

	t.Run("address is required for every kind", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			mutate func(*Fulfillment)
		}{
			{"country", func(f *Fulfillment) { f.CountryCode = "" }},
			{"city", func(f *Fulfillment) { f.City = "" }},
			{"zipcode", func(f *Fulfillment) { f.Zipcode = "" }},
			{"street", func(f *Fulfillment) { f.Street = "" }},
		} {
			f := homeDelivery()
			tc.mutate(&f)
			assert.False(t, f.Complete(), "missing %s", tc.name)
		}
	})

	t.Run("acs pickup needs station external id and branch", func(t *testing.T) {
		f := homeDelivery()
		f.Kind = FulfillmentPickupPoint
		f.ProviderCode = "acs"
		assert.False(t, f.Complete())
		f.AcsStationExternalID = "SP123"
		assert.False(t, f.Complete())
		f.AcsStationBranch = "12"
		assert.True(t, f.Complete())
	})

	t.Run("boxnow pickup needs locker id", func(t *testing.T) {
		f := homeDelivery()
		f.Kind = FulfillmentPickupPoint
		f.ProviderCode = "boxnow"
		assert.False(t, f.Complete())
		f.BoxnowLockerID = "842"
		assert.True(t, f.Complete())
	})

	t.Run("unknown pickup provider or kind is incomplete", func(t *testing.T) {
		f := homeDelivery()
		f.Kind = FulfillmentPickupPoint
		f.ProviderCode = "carrier_pigeon"
		assert.False(t, f.Complete())
		f.Kind = "teleport"
		assert.False(t, f.Complete())
	})
}

func TestRecompute(t *testing.T) {
	t.Run("empty session is incomplete", func(t *testing.T) {
		s := NewSession("public", "shop.test", "ucp", "cart-1")
		s.Recompute()
		assert.Equal(t, StatusIncomplete, s.Status)
		assert.Len(t, s.Missing(), 3)
	})

	t.Run("all inputs present becomes ready", func(t *testing.T) {
		s := NewSession("public", "shop.test", "ucp", "cart-1")
		s.Buyer = completeBuyer()
		s.Fulfillment = homeDelivery()
		s.PayWayID = 1
		s.Recompute()
		assert.Equal(t, StatusReadyForComplete, s.Status)
		assert.Empty(t, s.Missing())
	})

	t.Run("removing input drops back to incomplete", func(t *testing.T) {
		s := NewSession("public", "shop.test", "ucp", "cart-1")
		s.Buyer = completeBuyer()
		s.Fulfillment = homeDelivery()
		s.PayWayID = 1
		s.Recompute()
		s.Buyer.Email = ""
		s.Recompute()
		assert.Equal(t, StatusIncomplete, s.Status)
		assert.Contains(t, s.Missing()[0], "buyer")
	})

	t.Run("never leaves escalation or terminal states", func(t *testing.T) {
		for _, status := range []Status{
			StatusRequiresEscalation, StatusCompleteInProgress,
			StatusCompleted, StatusCanceled,
		} {
			s := NewSession("public", "shop.test", "ucp", "cart-1")
			s.Buyer = completeBuyer()
			s.Fulfillment = homeDelivery()
			s.PayWayID = 1
			s.Status = status
			s.Recompute()
			assert.Equal(t, status, s.Status)
		}
	})
}

func TestTerminal(t *testing.T) {
	s := NewSession("public", "shop.test", "ucp", "cart-1")
	assert.False(t, s.Terminal())
	s.Status = StatusRequiresEscalation
	assert.False(t, s.Terminal())
	s.Status = StatusCompleted
	assert.True(t, s.Terminal())
	s.Status = StatusCanceled
	assert.True(t, s.Terminal())
}
