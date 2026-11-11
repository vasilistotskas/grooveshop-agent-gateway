package checkout

import (
	"testing"
	"time"

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
			{"street number", func(f *Fulfillment) { f.StreetNumber = "" }},
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

	t.Run("acp sessions are ready without a pay way", func(t *testing.T) {
		s := NewSession("public", "shop.test", "acp", "cart-1")
		s.Buyer = completeBuyer()
		s.Fulfillment = homeDelivery()
		s.Recompute()
		assert.Equal(t, StatusReadyForComplete, s.Status)
		assert.Empty(t, s.Missing())
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

func TestPlacedSessionLifeCountsFromPlacement(t *testing.T) {
	now := time.Now()
	placed := func(age time.Duration, status Status) *Session {
		return &Session{Status: status, PlacedAt: now.Add(-age)}
	}

	// Open sessions live activeTTL from each save, and so does one just
	// marked in progress: nothing was sent for it yet, and if nothing
	// ever says what became of it, it must not claim an order for a day.
	// One an order exists for, or was sent for, is kept terminalTTL:
	// while it is held a caller is told what became of the checkout.
	for status, want := range map[Status]time.Duration{
		StatusIncomplete:         activeTTL,
		StatusReadyForComplete:   activeTTL,
		StatusCompleteInProgress: activeTTL,
		StatusRequiresEscalation: terminalTTL,
		StatusCompleted:          terminalTTL,
		StatusCanceled:           terminalTTL,
	} {
		assert.Equal(t, want, (&Session{Status: status}).ttl(now), status)
	}
	inProgress := Session{Status: StatusCompleteInProgress}
	// Its order was sent and never answered: an order may exist.
	unconfirmed := inProgress
	unconfirmed.Unconfirmed = true
	assert.Equal(t, terminalTTL, unconfirmed.ttl(now))
	// Its order was placed and the session could not be completed.
	named := inProgress
	named.OrderUUID = "b9be45e5-6062-4976-ae7b-2c31eb2ad689"
	assert.Equal(t, terminalTTL, named.ttl(now))
	// A placed session ends terminalTTL after it was placed.
	assert.Equal(t, terminalTTL-time.Hour,
		placed(time.Hour, StatusRequiresEscalation).ttl(now))
	assert.Equal(t, 10*time.Minute,
		placed(terminalTTL-10*time.Minute, StatusRequiresEscalation).ttl(now))
	assert.Equal(t, time.Hour,
		placed(terminalTTL-time.Hour, StatusCompleted).ttl(now))
	assert.LessOrEqual(t,
		placed(terminalTTL, StatusRequiresEscalation).ttl(now), time.Duration(0))
}

// Only an open session may be written by a call that is not placing it:
// a placed, escalated, in-progress or canceled one must not change.
func TestOpen(t *testing.T) {
	open := map[Status]bool{
		StatusIncomplete:         true,
		StatusReadyForComplete:   true,
		StatusCompleteInProgress: false,
		StatusRequiresEscalation: false,
		StatusCompleted:          false,
		StatusCanceled:           false,
	}
	for status, want := range open {
		assert.Equal(t, want, (&Session{Status: status}).Open(), status)
	}
}

// Every session is open, canceled, or may have an order: the three
// never overlap, so a decision made on one of them covers each status
// once.
func TestMayHaveOrder(t *testing.T) {
	may := map[Status]bool{
		StatusIncomplete:         false,
		StatusReadyForComplete:   false,
		StatusCompleteInProgress: true,
		StatusRequiresEscalation: true,
		StatusCompleted:          true,
		StatusCanceled:           false,
	}
	for status, want := range may {
		s := &Session{Status: status}
		assert.Equal(t, want, s.MayHaveOrder(), status)
		states := 0
		for _, in := range []bool{
			s.Open(), s.MayHaveOrder(), status == StatusCanceled,
		} {
			if in {
				states++
			}
		}
		assert.Equal(t, 1, states, status)
	}
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
