package ucp

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/media"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Wire types mirror internal/ucp/spec/2026-08-25 (amounts are integer
// minor units; field sets follow shopping/checkout.json and friends).

type Envelope struct {
	Version string `json:"version"`
	// Capabilities declares what is active for this response: the
	// negotiated checkout capability and its negotiated extensions.
	Capabilities    map[string][]CapabilityRef  `json:"capabilities"`
	PaymentHandlers map[string][]PaymentHandler `json:"payment_handlers"`
}

// ErrorResponse is common/types/error_response: returned in place of a
// checkout or an order when no valid resource can be established.
type ErrorResponse struct {
	UCP         ErrorEnvelope `json:"ucp"`
	Messages    []Message     `json:"messages"`
	ContinueURL string        `json:"continue_url,omitempty"`
}

// ErrorEnvelope is the ucp member of an error response.
type ErrorEnvelope struct {
	Version      string                     `json:"version"`
	Status       string                     `json:"status"`
	Capabilities map[string][]CapabilityRef `json:"capabilities,omitempty"`
}

// OrderNotFound is get_order's answer for an order it does not serve
// the caller, in the words of the spec's own example (order capability,
// Get Order, Error Responses): an id that names no order, and one that
// names an order the calling platform did not place, are answered alike.
func OrderNotFound(
	capabilities map[string][]CapabilityRef,
) *ErrorResponse {
	return &ErrorResponse{
		UCP: ErrorEnvelope{
			Version: Version, Status: "error", Capabilities: capabilities,
		},
		Messages: []Message{{
			Type: "error", Code: "not_found",
			Text: "Order not found.", Severity: "unrecoverable",
		}},
	}
}

// CapabilitiesIncompatible is the negotiation-failure response: the
// platform's profile is valid, but it shares no version of the
// capability the operation needs. continueURL is the web handoff.
func CapabilitiesIncompatible(continueURL string) *ErrorResponse {
	return &ErrorResponse{
		UCP: ErrorEnvelope{Version: Version, Status: "error"},
		Messages: []Message{{
			Type: "error", Code: "capabilities_incompatible",
			Text:     "No compatible capabilities in the intersection",
			Severity: "unrecoverable",
		}},
		ContinueURL: continueURL,
	}
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
	// Path is the RFC 9535 JSONPath, into the response, of what the
	// message is about.
	Path string `json:"path,omitempty"`
	Text string `json:"content,omitempty"`
	// Severity is required on error messages (message_error.json) and
	// absent on info and warning ones.
	Severity string `json:"severity,omitempty"`
}

// Discounts is the discount extension's member (shopping/discount.json
// discounts_object), on a checkout whose platform negotiated it.
type Discounts struct {
	// Codes are the codes the checkout holds, echoed so that a platform
	// sending the checkout back on update keeps them.
	Codes   []string          `json:"codes,omitempty"`
	Applied []AppliedDiscount `json:"applied,omitempty"`
}

// AppliedDiscount is one discount the store applies: Code is empty for an
// automatic one. Amount is positive; its totals row is negative.
type AppliedDiscount struct {
	Code      string `json:"code,omitempty"`
	Title     string `json:"title"`
	Amount    int64  `json:"amount"`
	Automatic bool   `json:"automatic,omitempty"`
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
	Discounts   *Discounts         `json:"discounts,omitempty"`
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
	env  string
	site storefront.Site
}

func NewBuilder(
	dj *django.Client, mediaURLTemplate, assetsHost, env string,
	site storefront.Site,
) *Builder {
	return &Builder{
		dj: dj, mediaURLTemplate: mediaURLTemplate, assetsHost: assetsHost,
		env: env, site: site,
	}
}

// BuildCheckout renders the current session state, fetching the cart for
// fresh line items and computing totals server-side (items + delivery +
// payment fee).
func (b *Builder) BuildCheckout(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	capabilities map[string][]CapabilityRef,
) (*Checkout, error) {
	pricing, _, err := checkout.ComputePricing(ctx, b.dj, t, s)
	if err != nil {
		return nil, fmt.Errorf("ucp: pricing: %w", err)
	}
	links := b.site.Links(t.Domain, django.Language(ctx, t.TenantConfig))

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
			Capabilities:    capabilities,
			PaymentHandlers: handlers,
		},
		ID: s.ID,
		// The spec types line_items as an array; keep it [] over null even
		// for an empty cart.
		LineItems: []LineItem{},
		Status:    string(s.Status),
		Currency:  t.DefaultCurrency,
		Links: []Link{
			{Type: "terms_of_service", URL: links.Terms()},
			{Type: "privacy_policy", URL: links.Privacy()},
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
			ID: strconv.FormatInt(line.LineID, 10),
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
	if _, ok := capabilities[CapabilityDiscount]; ok {
		renderDiscounts(s, pricing, out)
	}

	switch s.Status {
	case checkout.StatusRequiresEscalation:
		// continue_url is REQUIRED whenever the status is
		// requires_escalation. With a hosted PSP page it is that page.
		// With an order but no page (the link could not be created) it is
		// the order, never the cart: the cart is kept until payment, and
		// claiming it would let the buyer check out a second order. The
		// status is set only once the order exists, so one of the two is
		// always there.
		switch {
		case s.PaymentURL != "":
			out.ContinueURL = s.PaymentURL
			out.Messages = append(out.Messages, Message{
				Type: "info", Code: "requires_buyer_review",
				Text: "The buyer must open continue_url to authorize " +
					"payment on the store's hosted checkout.",
			})
		case s.OrderUUID != "":
			out.ContinueURL = links.OrderSuccess(s.OrderUUID)
			out.Messages = append(out.Messages, Message{
				Type: "error", Code: "payment_link_unavailable",
				Text: "The order is placed but its payment link could not " +
					"be created; call complete_checkout again to retry.",
				Severity: "recoverable",
			})
		}
	case checkout.StatusIncomplete:
		// No continue_url while the checkout is open, though the spec
		// SHOULDs one: the only page there is the claim page for its
		// cart, and a link handed out outlives the status. Once this
		// checkout places an order that awaits its online payment, Django
		// keeps the cart, and refuses a second order from it only when
		// an agent places it: a buyer who opened the earlier link could
		// order the same cart again on the web.
		for _, m := range s.Missing() {
			out.Messages = append(out.Messages,
				Message{Type: "info", Code: "missing_input", Text: m})
		}
	}
	if s.OrderUUID != "" && s.Status == checkout.StatusCompleted {
		out.Order = &OrderConfirmation{
			ID:           s.OrderUUID,
			Label:        fmt.Sprintf("Order %d", s.OrderID),
			PermalinkURL: links.OrderSuccess(s.OrderUUID),
		}
	}
	return out, nil
}

// renderDiscounts fills the discount extension's member: the codes the
// checkout holds, and the discounts the cart applies, each with its own
// amount (a placed order's arrive as one sum, so none are listed then).
// Each refused code is a warning message at its place in codes, as the
// extension has it, carrying the rejection's code.
func renderDiscounts(
	s *checkout.Session, pricing *checkout.Pricing, out *Checkout,
) {
	d := &Discounts{}
	held := func(code string) {
		if code != "" && !slices.ContainsFunc(d.Codes, func(c string) bool {
			return strings.EqualFold(strings.TrimSpace(c), code)
		}) {
			d.Codes = append(d.Codes, code)
		}
	}
	if s.OrderUUID != "" {
		// A placed order's discounts are one sum: its codes are those the
		// checkout submitted.
		for _, code := range s.DiscountCodes {
			held(code)
		}
	} else {
		// The codes are the cart's, so a platform that sends the checkout
		// back keeps exactly them, and one that leaves them out clears
		// them: the coupon the cart holds first (applied, or refused
		// now), whoever attached it, then the codes the store refused
		// when they were submitted, which their warnings point at. A
		// submitted code the cart no longer holds is not listed: sent
		// back, it would replace the coupon the cart holds now.
		for _, disc := range pricing.Discounts {
			held(disc.Code)
		}
		for _, refused := range pricing.RefusedCoupons {
			held(refused.Code)
		}
		for _, rejected := range s.RejectedDiscounts {
			held(rejected.Code)
		}
	}
	for _, disc := range pricing.Discounts {
		title := disc.Name
		if title == "" {
			title = disc.Code
		}
		d.Applied = append(d.Applied, AppliedDiscount{
			Code: disc.Code, Title: title, Amount: disc.Amount,
			Automatic: disc.Code == "",
		})
	}
	for _, rej := range checkout.Rejections(s, pricing) {
		content := rej.Message
		if content == "" {
			content = fmt.Sprintf(
				"Discount code %q could not be applied.", rej.Code)
		}
		m := Message{Type: "warning", Code: rej.Reason, Text: content}
		if i := slices.IndexFunc(d.Codes, func(c string) bool {
			return strings.EqualFold(strings.TrimSpace(c), rej.Code)
		}); i >= 0 {
			m.Path = fmt.Sprintf("$.discounts.codes[%d]", i)
		}
		out.Messages = append(out.Messages, m)
	}
	if len(d.Codes)+len(d.Applied) > 0 {
		out.Discounts = d
	}
}
