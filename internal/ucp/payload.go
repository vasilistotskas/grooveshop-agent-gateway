package ucp

import (
	"context"
	"fmt"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/media"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Wire types mirror testdata/schemas/ucp/2026-04-08 (amounts are integer
// minor units; field sets follow shopping/checkout.json and friends).

type Envelope struct {
	Version         string                      `json:"version"`
	PaymentHandlers map[string][]PaymentHandler `json:"payment_handlers"`
}

type PaymentHandler struct {
	ID      string         `json:"id"`
	Version string         `json:"version"`
	Config  map[string]any `json:"config,omitempty"`
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
}

func NewBuilder(dj *django.Client, mediaURLTemplate string) *Builder {
	return &Builder{dj: dj, mediaURLTemplate: mediaURLTemplate}
}

// paymentHandlers advertises what this tenant can accept. Stripe's
// tokenized handler appears only when the tenant's agentic flag is on
// (ships with the delegated-payment milestone); the Viva hosted flow needs
// no handler — it rides requires_escalation + continue_url.
func paymentHandlers(t *tenant.Tenant) map[string][]PaymentHandler {
	return map[string][]PaymentHandler{}
}

// BuildCheckout renders the current session state, fetching the cart for
// fresh line items and computing totals server-side (items + delivery +
// payment fee).
func (b *Builder) BuildCheckout(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
) (*Checkout, error) {
	cart, err := b.dj.GetCart(ctx, t.Domain, t.DefaultLocale, s.CartID)
	if err != nil {
		return nil, fmt.Errorf("ucp: cart fetch: %w", err)
	}

	out := &Checkout{
		UCP: Envelope{
			Version:         Version,
			PaymentHandlers: paymentHandlers(t),
		},
		ID: s.ID,
		// The spec types line_items as an array; keep it [] over null even
		// for an empty cart.
		LineItems: []LineItem{},
		Status:    string(s.Status),
		Currency:  t.DefaultCurrency,
		Links: []Link{
			{Type: "terms_of_service",
				URL: fmt.Sprintf("https://%s/terms-and-conditions", t.Domain)},
			{Type: "privacy_policy",
				URL: fmt.Sprintf("https://%s/privacy-policy", t.Domain)},
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

	var itemsSubtotal int64
	for _, it := range cart.Items {
		tr := it.Product.Translations[t.DefaultLocale]
		unit, err := money.MinorUnits(it.FinalPrice.String())
		if err != nil {
			return nil, err
		}
		lineTotal, err := money.MinorUnits(it.TotalPrice.String())
		if err != nil {
			return nil, err
		}
		itemsSubtotal += lineTotal
		out.LineItems = append(out.LineItems, LineItem{
			ID: fmt.Sprintf("%d", it.ID),
			Item: Item{
				ID:    fmt.Sprintf("%d", it.Product.ID),
				Title: tr.Name,
				Price: unit,
				ImageURL: media.ImageURL(b.mediaURLTemplate,
					t.Domain, t.SchemaName, it.Product.MainImagePath),
			},
			Quantity: it.Quantity,
			Totals: []LineItemTotal{
				{Type: "subtotal", Amount: lineTotal},
				{Type: "total", Amount: lineTotal},
			},
		})
	}

	total := itemsSubtotal
	out.Totals = append(out.Totals,
		LineItemTotal{Type: "subtotal", Amount: itemsSubtotal})

	if fee, ok, err := b.deliveryFee(ctx, t, s, cart); err == nil && ok {
		out.Totals = append(out.Totals,
			LineItemTotal{Type: "fulfillment", Amount: fee})
		total += fee
	}
	if fee, ok, err := b.paymentFee(ctx, t, s, cart); err == nil && ok {
		out.Totals = append(out.Totals, LineItemTotal{
			Type: "fee", DisplayText: "Payment method fee", Amount: fee})
		total += fee
	}
	out.Totals = append(out.Totals,
		LineItemTotal{Type: "total", Amount: total})

	switch s.Status {
	case checkout.StatusRequiresEscalation:
		// The buyer authorizes payment on the hosted page (Viva).
		out.ContinueURL = s.PaymentURL
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
			ID:    s.OrderUUID,
			Label: fmt.Sprintf("Order %d", s.OrderID),
			PermalinkURL: fmt.Sprintf("https://%s/checkout/success/%s",
				t.Domain, s.OrderUUID),
		}
	}
	return out, nil
}

// deliveryFee resolves the chosen shipping option's price.
func (b *Builder) deliveryFee(
	ctx context.Context, t *tenant.Tenant,
	s *checkout.Session, cart *django.Cart,
) (int64, bool, error) {
	f := s.Fulfillment
	if f.ProviderCode == "" || f.Kind == "" || f.CountryCode == "" {
		return 0, false, nil
	}
	opts, err := b.dj.ShippingOptions(ctx, t.Domain, t.DefaultLocale,
		django.ShippingQuery{
			CountryCode:      f.CountryCode,
			OrderValueAmount: cart.TotalPrice.String(),
			Currency:         t.DefaultCurrency,
		})
	if err != nil {
		return 0, false, err
	}
	for _, o := range opts {
		if o.ProviderCode == f.ProviderCode && o.Kind == f.Kind {
			fee, err := money.MinorUnits(o.Price.String())
			return fee, err == nil, err
		}
	}
	return 0, false, nil
}

// paymentFee resolves the chosen pay way's fee, waived above its free
// threshold.
func (b *Builder) paymentFee(
	ctx context.Context, t *tenant.Tenant,
	s *checkout.Session, cart *django.Cart,
) (int64, bool, error) {
	if s.PayWayID <= 0 {
		return 0, false, nil
	}
	pw, err := b.dj.PayWayByID(ctx, t.Domain, t.DefaultLocale, s.PayWayID)
	if err != nil {
		return 0, false, err
	}
	cost, err := money.MinorUnits(pw.Cost.String())
	if err != nil {
		return 0, false, err
	}
	threshold, err := money.MinorUnits(pw.FreeThreshold.String())
	if err != nil {
		return 0, false, err
	}
	cartTotal, err := money.MinorUnits(cart.TotalPrice.String())
	if err != nil {
		return 0, false, err
	}
	if threshold > 0 && cartTotal >= threshold {
		return 0, false, nil
	}
	if cost == 0 {
		return 0, false, nil
	}
	return cost, true, nil
}
