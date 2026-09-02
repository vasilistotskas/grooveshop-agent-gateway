package ucp

import (
	"context"
	"fmt"
	"strconv"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/media"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Wire types mirror testdata/schemas/ucp/2026-08-25 (amounts are integer
// minor units; field sets follow shopping/checkout.json and friends).

type Envelope struct {
	Version         string                      `json:"version"`
	PaymentHandlers map[string][]PaymentHandler `json:"payment_handlers"`
}

type PaymentHandler struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	// Spec and Schema are optional in a business profile but published
	// so a platform can fetch and compose the instrument shapes during
	// negotiation. Omitted in checkout responses, where the containing
	// declaration is already authoritative.
	Spec   string `json:"spec,omitempty"`
	Schema string `json:"schema,omitempty"`
	// AvailableInstruments narrows what this handler accepts. In a
	// business profile it is the merchant's standing capability; in a
	// response it is the set resolved for that checkout, which a
	// platform MUST treat as authoritative.
	AvailableInstruments []AvailableInstrument `json:"available_instruments,omitempty"`
	Config               map[string]any        `json:"config,omitempty"`
}

type Item struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Price    int64  `json:"price"`
	ImageURL string `json:"image_url,omitempty"`
}

type LineItemTotal struct {
	Type        string `json:"type"`
	DisplayText string `json:"display_text,omitempty"`
	Amount      int64  `json:"amount"`
}

type LineItem struct {
	ID       string          `json:"id"`
	Item     Item            `json:"item"`
	Quantity int             `json:"quantity"`
	Totals   []LineItemTotal `json:"totals"`
}

type Buyer struct {
	FirstName   string `json:"first_name,omitempty"`
	LastName    string `json:"last_name,omitempty"`
	Email       string `json:"email,omitempty"`
	PhoneNumber string `json:"phone_number,omitempty"`
}

type Link struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

type OrderConfirmation struct {
	ID           string `json:"id"`
	Label        string `json:"label,omitempty"`
	PermalinkURL string `json:"permalink_url"`
}

type Message struct {
	Type string `json:"type"`
	Code string `json:"code,omitempty"`
	Text string `json:"content,omitempty"`
}

// Checkout is the UCP checkout session response payload.
type Checkout struct {
	UCP         Envelope           `json:"ucp"`
	ID          string             `json:"id"`
	LineItems   []LineItem         `json:"line_items"`
	Buyer       *Buyer             `json:"buyer,omitempty"`
	Status      string             `json:"status"`
	Currency    string             `json:"currency"`
	Totals      []LineItemTotal    `json:"totals"`
	Links       []Link             `json:"links"`
	ContinueURL string             `json:"continue_url,omitempty"`
	Order       *OrderConfirmation `json:"order,omitempty"`
	Messages    []Message          `json:"messages,omitempty"`
}

// Builder renders sessions into wire payloads with fresh cart data.
type Builder struct {
	dj               *django.Client
	mediaURLTemplate string
	assetsHost       string
	// env is the payment handler's environment (config.PaymentHandlerEnv),
	// surfaced to platforms so they keep test traffic out of live orders.
	env string
}

func NewBuilder(
	dj *django.Client, mediaURLTemplate, assetsHost, env string,
) *Builder {
	return &Builder{
		dj: dj, mediaURLTemplate: mediaURLTemplate, assetsHost: assetsHost,
		env: env,
	}
}

// BuildCheckout renders the current session state, fetching the cart for
// fresh line items and computing totals server-side (items + delivery +
// payment fee).
func (b *Builder) BuildCheckout(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
) (*Checkout, error) {
	pricing, _, err := checkout.ComputePricing(ctx, b.dj, t, s)
	if err != nil {
		return nil, fmt.Errorf("ucp: pricing: %w", err)
	}

	// The response declaration is authoritative for this checkout: it
	// lists what the agent may still select.
	//
	// Readiness is NOT narrowed by which methods an agent can settle.
	// In UCP `ready_for_complete` means the inputs are collected and the
	// platform should call complete; whether completion then needs the
	// buyer at the PSP is discovered by completing, which returns
	// requires_escalation with the PSP's own continue_url. Downgrading
	// here would send the agent to a browser instead of placing the
	// order, and no order would exist for the buyer to pay for. ACP's
	// ready_for_payment carries the narrower "agent can pay now"
	// meaning; the vocabularies do not map onto each other.
	handlers := responsePaymentHandlers(t, b.env)

	out := &Checkout{
		UCP: Envelope{
			Version:         Version,
			PaymentHandlers: handlers,
		},
		ID: s.ID,
		// The spec types line_items as an array; keep it [] over null even
		// for an empty cart.
		LineItems: []LineItem{},
		Status:    string(s.Status),
		Currency:  t.DefaultCurrency,
		Links: []Link{
			{Type: "terms_of_service", URL: storefront.Terms(t.Domain)},
			{Type: "privacy_policy", URL: storefront.Privacy(t.Domain)},
		},
	}

	if s.Buyer != (checkout.Buyer{}) {
		out.Buyer = &Buyer{
			FirstName:   s.Buyer.FirstName,
			LastName:    s.Buyer.LastName,
			Email:       s.Buyer.Email,
			PhoneNumber: s.Buyer.Phone,
		}
	}

	for _, line := range pricing.Lines {
		out.LineItems = append(out.LineItems, LineItem{
			ID: strconv.FormatInt(line.CartItemID, 10),
			Item: Item{
				ID:    strconv.FormatInt(line.ProductID, 10),
				Title: line.Title,
				Price: line.UnitMinor,
				ImageURL: media.ImageURL(b.mediaURLTemplate,
					media.Host(t.AssetsDomain, b.assetsHost),
					t.SchemaName, line.ImagePath),
			},
			Quantity: line.Quantity,
			Totals: []LineItemTotal{
				{Type: "subtotal", Amount: line.TotalMinor},
				{Type: "total", Amount: line.TotalMinor},
			},
		})
	}

	out.Totals = append(out.Totals,
		LineItemTotal{Type: "subtotal", Amount: pricing.ItemsSubtotal})
	if pricing.DiscountTotal > 0 {
		// UCP types discount amounts as strictly negative signed values
		// (total.json: exclusiveMaximum 0).
		out.Totals = append(out.Totals, LineItemTotal{
			Type: "discount", DisplayText: "Discount",
			Amount: -pricing.DiscountTotal})
	}
	if pricing.HasDelivery {
		out.Totals = append(out.Totals,
			LineItemTotal{Type: "fulfillment", Amount: pricing.DeliveryFee})
	}
	if pricing.HasPaymentFee {
		out.Totals = append(out.Totals, LineItemTotal{
			Type: "fee", DisplayText: "Payment method fee",
			Amount: pricing.PaymentFee})
	}
	out.Totals = append(out.Totals,
		LineItemTotal{Type: "total", Amount: pricing.Total})

	switch s.Status {
	case checkout.StatusRequiresEscalation:
		// continue_url is REQUIRED whenever the status is
		// requires_escalation. A hosted PSP page exists only once the
		// buyer reached payment; before that — including a store with no
		// agent-completable method at all — the honest handoff is the
		// storefront's own claim page for this cart.
		out.ContinueURL = s.PaymentURL
		if out.ContinueURL == "" {
			out.ContinueURL = storefront.CartClaim(t.Domain, s.CartID)
		}
		out.Messages = append(out.Messages, Message{
			Type: "info", Code: "requires_buyer_review",
			Text: "The buyer must open continue_url to authorize payment " +
				"on the store's hosted checkout.",
		})
	case checkout.StatusIncomplete:
		for _, m := range s.Missing() {
			out.Messages = append(out.Messages,
				Message{Type: "info", Code: "missing_input", Text: m})
		}
	}
	if s.OrderUUID != "" && s.Status == checkout.StatusCompleted {
		out.Order = &OrderConfirmation{
			ID:           s.OrderUUID,
			Label:        fmt.Sprintf("Order %d", s.OrderID),
			PermalinkURL: storefront.OrderSuccess(t.Domain, s.OrderUUID),
		}
	}
	return out, nil
}
