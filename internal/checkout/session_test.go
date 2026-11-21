package checkout

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
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
		Street: "Πανεπιστημίου 12",
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

	t.Run("the address is required", func(t *testing.T) {
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
			assert.False(t, f.HasAddress(), "missing %s", tc.name)
		}
	})

	t.Run("and a home delivery", func(t *testing.T) {
		f := homeDelivery()
		f.Kind = ""
		assert.True(t, f.HasAddress())
		assert.False(t, f.Complete())
	})
}

// A street line the store refused keeps the checkout from being ready
// until the address changes; a delivery to the same address keeps the
// refusal.
func TestARefusedStreetHoldsUntilTheAddressChanges(t *testing.T) {
	s := NewSession("public", "shop.test", ProtocolUCP, "cart-1")
	s.Buyer, s.PayWayID = completeBuyer(), 1
	s.Deliver(homeDelivery())
	s.DestinationRefused = &Refusal{Code: django.CodeStreetNumberMissing}
	s.Recompute()
	assert.Equal(t, StatusIncomplete, s.Status)
	assert.Equal(t, []Need{NeedDestination}, s.Missing())

	other := homeDelivery()
	other.ProviderCode = "elta"
	s.Deliver(other)
	assert.NotNil(t, s.DestinationRefused, "another carrier, the same address")

	other.Street = "Πανεπιστημίου 12Α"
	s.Deliver(other)
	assert.Nil(t, s.DestinationRefused)
	s.Recompute()
	assert.Equal(t, StatusReadyForComplete, s.Status)
}

func TestRecompute(t *testing.T) {
	t.Run("empty session is incomplete", func(t *testing.T) {
		s := NewSession("public", "shop.test", "ucp", "cart-1")
		s.Recompute()
		assert.Equal(t, StatusIncomplete, s.Status)
		assert.Equal(t, []Need{NeedBuyer, NeedAddress, NeedDelivery,
			NeedPayment}, s.Missing())
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
		assert.Equal(t, []Need{NeedBuyer}, s.Missing())
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
	life := func(s *Session) time.Duration {
		return s.keptUntil(now).Sub(now)
	}
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
		assert.Equal(t, want, life(&Session{Status: status}), status)
	}
	inProgress := Session{Status: StatusCompleteInProgress}
	// Its order was sent and never answered: an order may exist.
	unconfirmed := inProgress
	unconfirmed.Unconfirmed = true
	assert.Equal(t, terminalTTL, life(&unconfirmed))
	// Its order was placed and the session could not be completed.
	named := inProgress
	named.OrderUUID = "b9be45e5-6062-4976-ae7b-2c31eb2ad689"
	assert.Equal(t, terminalTTL, life(&named))
	// A placed session ends terminalTTL after it was placed.
	assert.Equal(t, terminalTTL-time.Hour,
		life(placed(time.Hour, StatusRequiresEscalation)))
	assert.Equal(t, 10*time.Minute,
		life(placed(terminalTTL-10*time.Minute, StatusRequiresEscalation)))
	assert.Equal(t, time.Hour,
		life(placed(terminalTTL-time.Hour, StatusCompleted)))
	assert.LessOrEqual(t,
		life(placed(terminalTTL, StatusRequiresEscalation)), time.Duration(0))
	// Unless the expiry it published is later: it is readable until then.
	late := placed(terminalTTL-time.Hour, StatusRequiresEscalation)
	late.ExpiresAt = now.Add(2 * time.Hour)
	assert.Equal(t, 2*time.Hour, life(late))
	// So is an open one, whose expiry another pod's clock set later.
	ahead := &Session{Status: StatusIncomplete,
		ExpiresAt: now.Add(activeTTL + time.Minute)}
	assert.Equal(t, activeTTL+time.Minute, life(ahead))
}

// A save publishes the session's own end as its expiry, in every state
// but one awaiting its payment, whose expiry is its order's; and the
// session is kept at least until the expiry it published.
func TestASaveExpiresTheSessionWhenItEnds(t *testing.T) {
	now := time.Now()
	for _, status := range []Status{
		StatusIncomplete, StatusReadyForComplete, StatusCompleteInProgress,
		StatusCompleted, StatusCanceled,
	} {
		s := &Session{Status: status}
		s.expire(now)
		assert.Equal(t, s.keptUntil(now), s.ExpiresAt, status)
	}

	placed := &Session{Status: StatusCompleted, PlacedAt: now.Add(-time.Hour)}
	placed.expire(now)
	assert.Equal(t, placed.PlacedAt.Add(terminalTTL), placed.ExpiresAt)

	decided := now.Add(40 * time.Minute)
	awaiting := &Session{Status: StatusRequiresEscalation,
		PlacedAt: now, ExpiresAt: decided}
	awaiting.expire(now.Add(time.Minute))
	assert.Equal(t, decided, awaiting.ExpiresAt, "the order's, not the store's")
	assert.False(t, awaiting.keptUntil(now).Before(awaiting.ExpiresAt))
}

// No save moves a published expiry earlier: a platform stops polling at
// the value it was given.
func TestTheExpiryNeverMovesEarlier(t *testing.T) {
	now := time.Now()
	s := &Session{Status: StatusIncomplete,
		ExpiresAt: now.Add(2 * activeTTL)}
	s.expire(now)
	assert.Equal(t, now.Add(2*activeTTL), s.ExpiresAt)

	awaiting := &Session{Status: StatusRequiresEscalation, PlacedAt: now,
		ExpiresAt: now.Add(time.Hour)}
	assert.False(t, awaiting.AwaitDecision(now.Add(30*time.Minute)))
	assert.Equal(t, now.Add(time.Hour), awaiting.ExpiresAt)
	assert.True(t, awaiting.AwaitDecision(now.Add(90*time.Minute)))
	assert.Equal(t, now.Add(90*time.Minute).UTC(), awaiting.ExpiresAt)
}

// A checkout awaiting its payment expires when Django will have decided
// its order; one whose order nothing will decide, when its session ends.
func TestAwaitDecision(t *testing.T) {
	now := time.Now().UTC()
	placed := now.Add(-time.Minute)
	s := &Session{Status: StatusRequiresEscalation, PlacedAt: placed,
		ExpiresAt: placed.Add(activeTTL)}
	decided := now.Add(45 * time.Minute)
	assert.True(t, s.AwaitDecision(decided))
	assert.Equal(t, decided, s.ExpiresAt)

	nothing := &Session{Status: StatusRequiresEscalation, PlacedAt: placed,
		ExpiresAt: placed.Add(activeTTL)}
	assert.True(t, nothing.AwaitDecision(time.Time{}))
	assert.Equal(t, placed.Add(terminalTTL), nothing.ExpiresAt)

	// Only a checkout awaiting its payment takes its order's decision.
	open := &Session{Status: StatusIncomplete, ExpiresAt: now}
	assert.False(t, open.AwaitDecision(decided))
	assert.Equal(t, now, open.ExpiresAt)
}

// The status a placed checkout takes from its order: the order's status
// is read before its payment's, a completed checkout stays completed,
// and a payment that lands after the order was canceled leaves the
// checkout canceled.
func TestFollowOrder(t *testing.T) {
	const (
		pending    = "PENDING"
		processing = "PROCESSING"
		canceled   = django.OrderStatusCanceled
		paid       = django.PaymentStatusCompleted
		refunded   = django.PaymentStatusRefunded
		failed     = django.PaymentStatusFailed
	)
	for _, tc := range []struct {
		from          Status
		order, paying string
		want          Status
	}{
		{StatusRequiresEscalation, pending, pending, StatusRequiresEscalation},
		{StatusRequiresEscalation, pending, failed, StatusRequiresEscalation},
		{StatusRequiresEscalation, pending, processing, StatusRequiresEscalation},
		{StatusRequiresEscalation, processing, paid, StatusCompleted},
		{StatusRequiresEscalation, "SHIPPED", paid, StatusCompleted},
		{StatusRequiresEscalation, "DELIVERED", refunded, StatusCompleted},
		{StatusRequiresEscalation, "COMPLETED",
			django.PaymentStatusPartRefund, StatusCompleted},
		{StatusRequiresEscalation, canceled, "CANCELED", StatusCanceled},
		{StatusRequiresEscalation, canceled, paid, StatusCanceled},
		{StatusRequiresEscalation, canceled, refunded, StatusCanceled},
		{StatusCompleteInProgress, processing, paid, StatusCompleted},
		{StatusCompleteInProgress, canceled, paid, StatusCompleteInProgress},
		{StatusCompleteInProgress, pending, pending, StatusCompleteInProgress},
		{StatusCompleted, canceled, "CANCELED", StatusCompleted},
		{StatusCanceled, processing, paid, StatusCanceled},
		{StatusReadyForComplete, processing, paid, StatusReadyForComplete},
	} {
		now := time.Now()
		s := &Session{Status: tc.from, PlacedAt: now,
			ExpiresAt: now.Add(time.Hour)}
		changed := s.FollowOrder(tc.order, tc.paying)
		name := string(tc.from) + " " + tc.order + "/" + tc.paying
		assert.Equal(t, tc.want, s.Status, name)
		assert.Equal(t, tc.want != tc.from, changed, name)
		if changed {
			// A decided checkout is readable until its session ends.
			assert.Equal(t, s.PlacedAt.Add(terminalTTL), s.ExpiresAt, name)
		}
	}
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

// A pickup delivery needs its point and country, never an address or a
// region; another point clears a refusal of the one before.
func TestAPickupDeliveryNeedsItsPoint(t *testing.T) {
	s := NewSession("public", "shop.test", ProtocolUCP, "cart-1")
	s.Buyer, s.PayWayID = completeBuyer(), 1
	s.Deliver(Fulfillment{Kind: FulfillmentPickupPoint})
	s.Recompute()
	assert.Equal(t, []Need{NeedPoint}, s.Missing())

	locker := Fulfillment{Kind: FulfillmentPickupPoint,
		ProviderCode: django.ShippingProviderBoxNow, CountryCode: "GR",
		BoxnowLockerID: "25"}
	s.Deliver(locker)
	s.RegionOptions = nil
	s.Recompute()
	assert.Empty(t, s.Missing())
	assert.Equal(t, StatusReadyForComplete, s.Status)

	acs := Fulfillment{Kind: FulfillmentPickupPoint,
		ProviderCode: django.ShippingProviderACS, CountryCode: "GR",
		AcsStationExternalID: "AAT"}
	assert.False(t, acs.Complete(), "an ACS station is a pair")
	acs.AcsStationBranch = "502"
	assert.True(t, acs.Complete())

	s.DestinationRefused = &Refusal{Code: django.CodePickupPointUnknown}
	s.Deliver(locker)
	assert.NotNil(t, s.DestinationRefused, "the same point")
	other := locker
	other.BoxnowLockerID = "26"
	s.Deliver(other)
	assert.Nil(t, s.DestinationRefused)
}

// A pickup checkout's quote names its point, as its order does, and none
// of a refused delivery's; without a delivery country, the options are
// listed for the buyer's locality.
func TestQuoteRequestOfAPickupAndOfALocality(t *testing.T) {
	s := NewSession("public", "shop.test", ProtocolUCP, "cart-1")
	s.Locality = Locality{CountryCode: "CY", PostalCode: "1010"}
	assert.Equal(t, django.QuoteRequest{CountryID: "CY"}, QuoteRequest(s))

	s.Deliver(Fulfillment{Kind: FulfillmentPickupPoint,
		ProviderCode: django.ShippingProviderBoxNow, CountryCode: "GR",
		BoxnowLockerID: "25"})
	assert.Equal(t, django.QuoteRequest{CountryID: "GR",
		ShippingKind:         FulfillmentPickupPoint,
		ShippingProviderCode: django.ShippingProviderBoxNow,
		BoxnowLockerID:       "25"},
		QuoteRequest(s))
}
