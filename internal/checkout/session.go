// Package checkout implements the protocol-neutral checkout session shared
// by the UCP MCP binding and the ACP REST surface: a pure state machine, a
// Redis store whose saves are compare-and-set, with per-session locking
// and idempotency claims, and the order-creation flow over the existing
// Django endpoints.
package checkout

import (
	"fmt"
	"slices"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// Protocol names the surface that owns a session; readiness rules differ
// per surface (see payWaySelectionRequired).
const (
	ProtocolUCP = "ucp"
	ProtocolACP = "acp"
)

// agentAttribution books what protocol's checkout fills or places as
// the agent's: the cart's source and the order's both read "AI agent".
func agentAttribution(protocol string) *django.Attribution {
	return &django.Attribution{AgentProtocol: protocol}
}

// Status values follow the UCP checkout lifecycle; the ACP layer maps them
// to its own vocabulary.
type Status string

const (
	StatusIncomplete         Status = "incomplete"
	StatusRequiresEscalation Status = "requires_escalation"
	StatusReadyForComplete   Status = "ready_for_complete"
	StatusCompleteInProgress Status = "complete_in_progress"
	StatusCompleted          Status = "completed"
	StatusCanceled           Status = "canceled"
)

// The delivery kinds of a checkout: to the buyer's address, or to a
// pickup point.
const (
	FulfillmentHomeDelivery = "home_delivery"
	FulfillmentPickupPoint  = django.ShippingKindPickupPoint
)

type Buyer struct {
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
}

func (b Buyer) Complete() bool {
	return b.FirstName != "" && b.LastName != "" &&
		b.Email != "" && b.Phone != ""
}

// Fulfillment is a checkout's delivery, as the order takes it, of one of
// two kinds (Kind) by a carrier (ProviderCode), in a country:
//   - home delivery, to a postal address: Street is the one street line
//     with its house number after the street ("Ερμού 12"), which Django
//     splits, and AddressLine2 what that line does not hold (a flat, a
//     floor);
//   - pickup at a point of the carrier's (a BOX NOW locker, an ACS station
//     and branch), with no address: Django delivers it to the point's.
type Fulfillment struct {
	Kind         string `json:"kind"`
	ProviderCode string `json:"providerCode"`

	CountryCode  string `json:"countryCode"`
	City         string `json:"city,omitempty"`
	Zipcode      string `json:"zipcode,omitempty"`
	Street       string `json:"street,omitempty"`
	AddressLine2 string `json:"addressLine2,omitempty"`
	RegionID     string `json:"regionId,omitempty"`

	AcsStationExternalID string `json:"acsStationExternalId,omitempty"`
	AcsStationBranch     string `json:"acsStationBranch,omitempty"`
	BoxnowLockerID       string `json:"boxnowLockerId,omitempty"`
	// Point is what the store's catalogue says of the pickup point, read
	// when it was chosen; nil for one the catalogue did not hold then.
	Point *Point `json:"point,omitempty"`
}

// Point is a pickup point as a buyer finds it.
type Point struct {
	Name       string `json:"name"`
	Street     string `json:"street,omitempty"`
	City       string `json:"city,omitempty"`
	PostalCode string `json:"postalCode,omitempty"`
}

// Pickup reports whether the delivery is to a pickup point.
func (f Fulfillment) Pickup() bool {
	return f.Kind == FulfillmentPickupPoint
}

// PickupPoint is the pickup point the fulfillment names, as both the
// quote and the order carry it, so the quote's digest is of the point the
// order is sent to.
func (f Fulfillment) PickupPoint() django.PickupPoint {
	return django.PickupPoint{
		AcsStationExternalID: f.AcsStationExternalID,
		AcsStationBranch:     f.AcsStationBranch,
		BoxnowLockerID:       f.BoxnowLockerID,
	}
}

// HasPoint reports whether the pickup point the order needs is named, by
// its carrier's own fields.
func (f Fulfillment) HasPoint() bool {
	switch f.ProviderCode {
	case django.ShippingProviderACS:
		return f.AcsStationExternalID != "" && f.AcsStationBranch != ""
	case django.ShippingProviderBoxNow:
		return f.BoxnowLockerID != ""
	}
	return false
}

// HasAddress reports whether the postal address a home delivery needs is
// present.
func (f Fulfillment) HasAddress() bool {
	return f.CountryCode != "" && f.City != "" && f.Zipcode != "" &&
		f.Street != ""
}

// Complete reports whether enough is present to place the order: a home
// delivery to its address, or a pickup at its point in a country.
func (f Fulfillment) Complete() bool {
	switch f.Kind {
	case FulfillmentHomeDelivery:
		return f.HasAddress()
	case FulfillmentPickupPoint:
		return f.CountryCode != "" && f.HasPoint()
	}
	return false
}

// sameDestination reports whether f and g deliver to the same place: the
// same postal address, or the same pickup point.
func (f Fulfillment) sameDestination(g Fulfillment) bool {
	return f.CountryCode == g.CountryCode && f.City == g.City &&
		f.Zipcode == g.Zipcode && f.Street == g.Street &&
		f.AddressLine2 == g.AddressLine2 &&
		f.PickupPoint() == g.PickupPoint()
}

// Session is the durable checkout state. Money totals are Django's quote
// at render time (ComputePricing); what is stored of them is what the
// checkout last showed (Shown), which its order is placed at.
type Session struct {
	ID       string `json:"id"`
	Protocol string `json:"protocol"` // ProtocolUCP | ProtocolACP
	Schema   string `json:"schema"`
	// Domain is the tenant storefront host at creation time, kept so
	// webhook payload permalinks render without a tenant lookup.
	Domain string `json:"domain"`

	// Revision names one stored state of the session: every save gives
	// it a new random one. Store.Save writes only over the revision it
	// read, so a request holding an older state cannot write. Not
	// UpdatedAt, and not a counter: two requests that read one state
	// and save in the same clock tick would write the same bytes, and
	// each would take the other's write for its own.
	Revision string `json:"revision"`

	Status Status `json:"status"`
	CartID string `json:"cartId"`

	Buyer       Buyer       `json:"buyer"`
	Fulfillment Fulfillment `json:"fulfillment"`
	PayWayID    int64       `json:"payWayId"`

	// DestinationRefused is Django's refusal of the delivery's destination
	// when the order was sent (django.DestinationRefusedError): a street
	// line with no house number it can read, or one that is a postcode; a
	// pickup point its carrier's catalogue does not hold. Until the
	// destination changes (Deliver), the checkout is not ready.
	DestinationRefused *Refusal `json:"destinationRefused,omitempty"`

	// Locality is where the buyer is, as the platform's checkout context
	// last said it: the pickup points near it are listed.
	Locality Locality `json:"locality,omitzero"`

	// RegionOptions are the delivery country's regions, as ResolveRegion
	// last read them; empty when it has none.
	RegionOptions []RegionOption `json:"regionOptions,omitempty"`

	// DiscountCodes is the last discount-code list the caller submitted
	// (replace semantics: each submission supersedes the previous one);
	// RejectedDiscounts records why codes from that submission were not
	// applied. What IS applied lives on the Django cart, never here.
	DiscountCodes     []string            `json:"discountCodes,omitempty"`
	RejectedDiscounts []DiscountRejection `json:"rejectedDiscounts,omitempty"`

	OrderID    int64  `json:"orderId,omitempty"`
	OrderUUID  string `json:"orderUuid,omitempty"`
	PaymentURL string `json:"paymentUrl,omitempty"`
	// PlacedAt ends the session terminalTTL later, whatever happens to it,
	// unless its ExpiresAt is later still.
	PlacedAt time.Time `json:"placedAt,omitzero"`
	// ExpiresAt is published as the checkout's expires_at: until then a
	// platform may read it, and the session is kept at least that long
	// (keptUntil). It never moves earlier than a value already published:
	// a platform stops polling at it. A checkout awaiting its payment
	// takes it from its order (AwaitDecision); every other state from
	// its own lifetime, set at each save (expire).
	ExpiresAt time.Time `json:"expiresAt"`
	// Shown is what the checkout last published while open: the total
	// the buyer was shown and the digest of the cart it was quoted for,
	// the one total and cart its order is placed at
	// (django.OrderCreate.ExpectedTotal, ExpectedCartDigest), since
	// Django creates the order only from them. Every render of an open
	// checkout sets it (Show), and the surfaces store it, a read only
	// under the free lock (Store.StoreShown). Nil until a render of the
	// checkout was stored.
	Shown *Shown `json:"shown,omitempty"`
	// Unconfirmed marks a session in progress whose order was sent and
	// never answered: the order may exist. One in progress without it,
	// and without an order, is a completion that is still running, or
	// one that ended before it could say what happened.
	Unconfirmed bool `json:"unconfirmed,omitempty"`

	// AppliedBy names the keyed request whose change this state holds
	// (ACP's update and cancel), written by the save that applied it. A
	// retry of that request finds itself here and is answered with the
	// session, not run again: its first attempt may have been saved and
	// then failed to answer. One slot: the latest such request.
	AppliedBy string `json:"appliedBy,omitempty"`

	// WebhookURL is the platform's order-update delivery target,
	// registered at create time.
	WebhookURL string `json:"webhookUrl,omitempty"`
	// Platform is the UCP platform that signed the session's creation:
	// its profile URL, normalized (ucp.NormalizeProfileURL). Only that
	// platform's signed calls reach the session (Serves), and only it
	// reads the order the session places (OrderLink.Platform). Empty for
	// a session created without a signature, which any caller of its
	// surface reaches.
	Platform string `json:"platform,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Caller is who asks for a session: the surface, and the platform whose
// verified signature the request carried ("" for none).
type Caller struct {
	Protocol string
	Platform string
}

// Serves reports whether c may reach the session: a session answers only
// on the surface that created it, and one a platform signed the creation
// of only to that platform.
func (s *Session) Serves(c Caller) bool {
	return s.Protocol == c.Protocol &&
		(s.Platform == "" || s.Platform == c.Platform)
}

// creator is the caller the session serves as itself: the request that
// holds it reads it again as such.
func (s *Session) creator() Caller {
	return Caller{Protocol: s.Protocol, Platform: s.Platform}
}

// Shown is what a render of an open checkout published, which its order
// is placed at: the total, and the quote's digest of what the order would
// be made of (Pricing.CartDigest), empty while the checkout has no
// delivery priced. The digest holds no pay way, so a checkout that names
// its method only at completion is placed from the cart it showed.
type Shown struct {
	Total      django.ExpectedTotal `json:"total"`
	CartDigest string               `json:"cartDigest,omitempty"`
}

// OfAnOrder reports whether an order can be placed at what was shown: a
// total with its cart's digest, which the quote gives once a delivery is
// priced.
func (sh *Shown) OfAnOrder() bool {
	return sh != nil && sh.CartDigest != ""
}

// Show records what a render of the session publishes, and reports
// whether it is another than what is stored: a new total, or the same
// total of another cart. Only an open checkout's: a placed one shows its
// order's, which nothing is placed at.
func (s *Session) Show(p *Pricing) bool {
	if !s.Open() || p.Order != nil {
		return false
	}
	shown := &Shown{
		Total:      django.ExpectedTotal{Amount: p.Total, Currency: p.Currency},
		CartDigest: p.CartDigest,
	}
	if s.Shown != nil && *s.Shown == *shown {
		return false
	}
	s.Shown = shown
	return true
}

// Terminal reports whether the session can no longer change.
func (s *Session) Terminal() bool {
	return s.Status == StatusCompleted || s.Status == StatusCanceled
}

// Open reports whether the session still takes what an order needs: it
// is neither placed, nor being placed, nor canceled.
func (s *Session) Open() bool {
	return s.Status == StatusIncomplete ||
		s.Status == StatusReadyForComplete
}

// MayHaveOrder reports whether an order exists for the session, or may:
// one is being placed (and stays "may" for good when Django never
// confirmed it), awaits its payment, or is completed. Such a session
// takes no update and no cancel: canceling the checkout would not
// cancel the order. Every session is open, canceled, or this.
func (s *Session) MayHaveOrder() bool {
	return s.Status == StatusCompleteInProgress ||
		s.Status == StatusRequiresEscalation ||
		s.Status == StatusCompleted
}

// keptLong reports whether the session is kept terminalTTL: it is
// terminal, or what the gateway knows says an order exists or was sent
// (it awaits payment, it names its order, or its order went unanswered).
// A session merely marked in progress is not: nothing has been sent for
// it yet. If its completion then dies, or Django refuses the order and
// the save that says so fails, it must not claim an order for a day.
func (s *Session) keptLong() bool {
	return s.Terminal() || s.Status == StatusRequiresEscalation ||
		s.OrderUUID != "" || s.Unconfirmed
}

// keptUntil is when the session, saved at now, ends. Calls keep an open
// session alive for activeTTL at a time, and a session just marked in
// progress has that long too. One an order exists for, or may
// (keptLong), is kept terminalTTL: while it is held, a caller is told
// what became of the checkout (do not order again, the order, its
// payment page), and a payment event finds it; once it is gone the id is
// only "no session". A placed one ends terminalTTL after it was placed,
// however often a caller holding its id touches it: it reads its order
// with the gateway's secret, past the limit on a guest's order uuid. It
// Every session is kept to its ExpiresAt if that is later: Django's
// decision on an unpaid order can come later than its day when its sweep
// is paused, and another pod's clock may have published a later one.
func (s *Session) keptUntil(now time.Time) time.Time {
	end := now.Add(activeTTL)
	switch {
	case !s.PlacedAt.IsZero():
		end = s.PlacedAt.Add(terminalTTL)
	case s.keptLong():
		end = now.Add(terminalTTL)
	}
	return later(end, s.ExpiresAt)
}

// expire sets ExpiresAt for a save at now: the session's own end. A
// checkout awaiting its payment is the exception: its expiry is when
// Django will have decided its order (AwaitDecision), which the store
// does not know.
func (s *Session) expire(now time.Time) {
	if s.Status == StatusRequiresEscalation {
		return
	}
	s.ExpiresAt = later(s.ExpiresAt, s.keptUntil(now))
}

// AwaitDecision publishes, for a checkout awaiting its payment, when
// Django will have decided its order (the order's paymentDecidedBy): a
// platform polls it until then, and reads the decision at it. A zero
// decidedBy is an order nothing will decide (its payment is being
// processed, a charge that did not match waits for the store, the sweep
// is off): the checkout is then readable until its session ends. The
// expiry never moves earlier. It reports whether it moved.
func (s *Session) AwaitDecision(decidedBy time.Time) bool {
	if s.Status != StatusRequiresEscalation {
		return false
	}
	if decidedBy.IsZero() {
		decidedBy = s.keptUntil(time.Now())
	}
	next := later(s.ExpiresAt, decidedBy.UTC())
	moved := !next.Equal(s.ExpiresAt)
	s.ExpiresAt = next
	return moved
}

// FollowOrder sets the status a placed checkout takes from its order's
// status and payment status, as Django reports them, and reports whether
// it changed. The order's status is read first:
//   - awaiting its payment (requires_escalation): canceled by the store,
//     paid or not, it is canceled; else paid (charged, refunds
//     included), it is completed; else it still awaits its payment;
//   - being placed (complete_in_progress, whose order an event already
//     names): paid and not canceled, it is completed; its completion
//     stores its own outcome over that (Flow.saveOverEvent);
//   - every other status, completed and canceled included, is not the
//     order's to change: a checkout completed at placement (a
//     collect-later method) stays completed when the order is later
//     canceled.
func (s *Session) FollowOrder(orderStatus, paymentStatus string) bool {
	canceled := orderStatus == django.OrderStatusCanceled
	charged := slices.Contains(django.ChargedPaymentStatuses, paymentStatus)
	next := s.Status
	switch s.Status {
	case StatusRequiresEscalation:
		switch {
		case canceled:
			next = StatusCanceled
		case charged:
			next = StatusCompleted
		}
	case StatusCompleteInProgress:
		if charged && !canceled {
			next = StatusCompleted
		}
	}
	if next == s.Status {
		return false
	}
	s.Status = next
	s.expire(time.Now())
	return true
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// payWaySelectionRequired reports whether readiness includes an explicit
// payment-method choice. UCP agents pick a pay way before complete; on
// ACP the merchant resolves a collect-later pay way at completion time,
// and Flow.admit refuses an online one there.
func (s *Session) payWaySelectionRequired() bool {
	return s.Protocol != ProtocolACP
}

// Recompute derives the pre-completion status from collected data. It
// never leaves escalation or terminal states — those transitions belong
// to the flow (order placed, payment confirmed, cancel).
func (s *Session) Recompute() {
	switch s.Status {
	case StatusRequiresEscalation, StatusCompleteInProgress,
		StatusCompleted, StatusCanceled:
		return
	}
	if len(s.Missing()) == 0 {
		s.Status = StatusReadyForComplete
		return
	}
	s.Status = StatusIncomplete
}

// Locality is a coarse place: a country and a postal code.
type Locality struct {
	CountryCode string `json:"countryCode,omitempty"`
	PostalCode  string `json:"postalCode,omitempty"`
}

// Deliver sets the checkout's delivery. A destination the store refused
// is refused no longer once the destination changes.
func (s *Session) Deliver(f Fulfillment) {
	if !s.Fulfillment.sameDestination(f) {
		s.DestinationRefused = nil
	}
	s.Fulfillment = f
}

// Need is one thing a checkout still needs before its order can be
// placed. Each surface says it in its own vocabulary.
type Need int

const (
	// NeedBuyer: the buyer's first and last name, email and phone.
	NeedBuyer Need = iota
	// NeedAddress: a home delivery's postal address (street line, city,
	// postal code, country).
	NeedAddress
	// NeedPoint: a pickup delivery's point.
	NeedPoint
	// NeedDestination: a destination the store takes, for the one it
	// refused (Session.DestinationRefused says why).
	NeedDestination
	// NeedRegion: the delivery region, one of Session.RegionChoices.
	NeedRegion
	// NeedDelivery: the carrier that delivers.
	NeedDelivery
	// NeedPayment: the payment method.
	NeedPayment
)

func (n Need) String() string {
	switch n {
	case NeedBuyer:
		return "the buyer's first and last name, email and phone"
	case NeedAddress:
		return "the delivery address (street line, city, postal code, " +
			"country)"
	case NeedPoint:
		return "the pickup point"
	case NeedDestination:
		return "another destination than the one the store refused"
	case NeedRegion:
		return "the delivery region"
	case NeedDelivery:
		return "the delivery option (carrier)"
	case NeedPayment:
		return "the payment method"
	}
	return fmt.Sprintf("need %d", int(n))
}

// Missing lists what still blocks completion, in that order.
func (s *Session) Missing() []Need {
	var out []Need
	if !s.Buyer.Complete() {
		out = append(out, NeedBuyer)
	}
	f := s.Fulfillment
	switch {
	case f.Pickup() && (f.CountryCode == "" || !f.HasPoint()):
		out = append(out, NeedPoint)
	case !f.Pickup() && !f.HasAddress():
		out = append(out, NeedAddress)
	case s.DestinationRefused != nil:
		out = append(out, NeedDestination)
	}
	if s.regionMissing() {
		out = append(out, NeedRegion)
	}
	if f.Kind != FulfillmentHomeDelivery && !f.Pickup() {
		out = append(out, NeedDelivery)
	}
	if s.PayWayID <= 0 && s.payWaySelectionRequired() {
		out = append(out, NeedPayment)
	}
	return out
}
