// Package checkout implements the protocol-neutral checkout session shared
// by the UCP MCP binding and the ACP REST surface: a pure state machine, a
// Redis store with per-session locking and completion idempotency, and the
// order-creation flow over the existing Django endpoints.
package checkout

import (
	"time"
)

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
	StreetNumber string `json:"streetNumber,omitempty"`

	AcsStationExternalID  string `json:"acsStationExternalId,omitempty"`
	AcsStationBranch      string `json:"acsStationBranch,omitempty"`
	BoxnowLockerID        string `json:"boxnowLockerId,omitempty"`
	BoxnowCompartmentSize int    `json:"boxnowCompartmentSize,omitempty"`
}

// Complete reports whether enough is present to place the order. Django
// requires the postal address even for pickup points (billing/contact).
func (f Fulfillment) Complete() bool {
	if f.CountryCode == "" || f.City == "" || f.Zipcode == "" ||
		f.Street == "" {
		return false
	}
	switch f.Kind {
	case FulfillmentHomeDelivery:
		return true
	case FulfillmentPickupPoint:
		switch f.ProviderCode {
		case "acs":
			return f.AcsStationExternalID != "" && f.AcsStationBranch != ""
		case "boxnow":
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
	Protocol string `json:"protocol"` // "ucp" | "acp"
	Schema   string `json:"schema"`
	// Domain is the tenant storefront host at creation time, kept so
	// webhook payload permalinks render without a tenant lookup.
	Domain string `json:"domain"`

	Status Status `json:"status"`
	CartID string `json:"cartId"`

	Buyer       Buyer       `json:"buyer"`
	Fulfillment Fulfillment `json:"fulfillment"`
	PayWayID    int64       `json:"payWayId"`

	// DiscountCodes is the last discount-code list the caller submitted
	// (replace semantics: each submission supersedes the previous one);
	// RejectedDiscounts records why codes from that submission were not
	// applied. What IS applied lives on the Django cart, never here.
	DiscountCodes     []string            `json:"discountCodes,omitempty"`
	RejectedDiscounts []DiscountRejection `json:"rejectedDiscounts,omitempty"`

	OrderID    int64  `json:"orderId,omitempty"`
	OrderUUID  string `json:"orderUuid,omitempty"`
	PaymentURL string `json:"paymentUrl,omitempty"`

	// WebhookURL and WebhookSecret are the platform's order-update
	// delivery target, registered at create time.
	WebhookURL    string `json:"webhookUrl,omitempty"`
	WebhookSecret string `json:"webhookSecret,omitempty"`

	Version   int       `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Terminal reports whether the session can no longer change.
func (s *Session) Terminal() bool {
	return s.Status == StatusCompleted || s.Status == StatusCanceled
}

// payWaySelectionRequired reports whether readiness includes an explicit
// payment-method choice. UCP agents pick a pay way before complete; on
// ACP payment is the platform's concern — the merchant resolves the pay
// way at completion time (delegated token, or cash on delivery).
func (s *Session) payWaySelectionRequired() bool {
	return s.Protocol != "acp"
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
	if s.Buyer.Complete() && s.Fulfillment.Complete() && payReady {
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
			"city, zipcode, street; plus the pickup point ids for "+
			"pickup_point deliveries)")
	}
	if s.PayWayID <= 0 && s.payWaySelectionRequired() {
		out = append(out, "payWayId (see get_payment_methods)")
	}
	return out
}
