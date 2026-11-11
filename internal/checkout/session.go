// Package checkout implements the protocol-neutral checkout session shared
// by the UCP MCP binding and the ACP REST surface: a pure state machine, a
// Redis store whose saves are compare-and-set, with per-session locking
// and idempotency claims, and the order-creation flow over the existing
// Django endpoints.
package checkout

import (
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

const (
	FulfillmentHomeDelivery = "home_delivery"
	FulfillmentPickupPoint  = "pickup_point"
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

// Fulfillment mirrors the Django order-create fields for delivery.
type Fulfillment struct {
	Kind         string `json:"kind"`
	ProviderCode string `json:"providerCode"`

	CountryCode  string `json:"countryCode"`
	City         string `json:"city"`
	Zipcode      string `json:"zipcode"`
	Street       string `json:"street"`
	StreetNumber string `json:"streetNumber"`
	RegionID     string `json:"regionId,omitempty" jsonschema:"the delivery region's code or name, required when the country has regions; a checkout without one lists them in its messages"`

	AcsStationExternalID string `json:"acsStationExternalId,omitempty"`
	AcsStationBranch     string `json:"acsStationBranch,omitempty"`
	BoxnowLockerID       string `json:"boxnowLockerId,omitempty"`
}

// Complete reports whether enough is present to place the order. Django
// requires the postal address even for pickup points (billing/contact),
// the street number included: without it a session would report itself
// ready and then fail at order creation.
func (f Fulfillment) Complete() bool {
	if f.CountryCode == "" || f.City == "" || f.Zipcode == "" ||
		f.Street == "" || f.StreetNumber == "" {
		return false
	}
	switch f.Kind {
	case FulfillmentHomeDelivery:
		return true
	case FulfillmentPickupPoint:
		switch f.ProviderCode {
		case django.ShippingProviderACS:
			return f.AcsStationExternalID != "" && f.AcsStationBranch != ""
		case django.ShippingProviderBoxNow:
			return f.BoxnowLockerID != ""
		}
		return false
	}
	return false
}

// Session is the durable checkout state. Money totals are derived fresh
// from the cart at render time, never stored.
type Session struct {
	ID       string `json:"id"`
	Protocol string `json:"protocol"` // ProtocolUCP | ProtocolACP
	Schema   string `json:"schema"`
	// Domain is the tenant storefront host at creation time, kept so
	// webhook payload permalinks render without a tenant lookup.
	Domain string `json:"domain"`

	// Version counts the times the session was stored. Store.Save
	// writes only over the version it read, so a request holding an
	// older one cannot write. A counter and not UpdatedAt: two pods'
	// clocks can stamp two saves alike.
	Version int64 `json:"version"`

	Status Status `json:"status"`
	CartID string `json:"cartId"`

	Buyer       Buyer       `json:"buyer"`
	Fulfillment Fulfillment `json:"fulfillment"`
	PayWayID    int64       `json:"payWayId"`

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
	// PlacedAt ends the session terminalTTL later, whatever happens to it.
	PlacedAt time.Time `json:"placedAt,omitzero"`

	// WebhookURL is the platform's order-update delivery target,
	// registered at create time.
	WebhookURL string `json:"webhookUrl,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
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

// ttl is how long the session may still live. Calls keep an open session alive,
// but not a placed one: it reads its order with the gateway's secret, past the
// limit on a guest's order uuid, so it must end long before that limit however
// often a caller holding its id touches it.
func (s *Session) ttl(now time.Time) time.Duration {
	ttl := activeTTL
	if s.Terminal() {
		ttl = terminalTTL
	}
	if s.PlacedAt.IsZero() {
		return ttl
	}
	return min(ttl, s.PlacedAt.Add(terminalTTL).Sub(now))
}

// payWaySelectionRequired reports whether readiness includes an explicit
// payment-method choice. UCP agents pick a pay way before complete; on
// ACP payment is the platform's concern — the merchant resolves the pay
// way at completion time (delegated token, or cash on delivery).
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
	payReady := s.PayWayID > 0 || !s.payWaySelectionRequired()
	if s.Buyer.Complete() && s.Fulfillment.Complete() &&
		!s.regionMissing() && payReady {
		s.Status = StatusReadyForComplete
		return
	}
	s.Status = StatusIncomplete
}

// Missing lists what still blocks completion, in agent-actionable terms.
func (s *Session) Missing() []string {
	var out []string
	if !s.Buyer.Complete() {
		out = append(out,
			"buyer (firstName, lastName, email, phone)")
	}
	if !s.Fulfillment.Complete() {
		out = append(out, "fulfillment (kind, providerCode, countryCode, "+
			"city, zipcode, street, streetNumber; plus the pickup point "+
			"ids for "+
			"pickup_point deliveries)")
	}
	if choices := s.RegionChoices(); choices != "" {
		out = append(out, "fulfillment.regionId, one of: "+choices)
	}
	if s.PayWayID <= 0 && s.payWaySelectionRequired() {
		out = append(out, "payWayId (see get_payment_methods)")
	}
	return out
}
