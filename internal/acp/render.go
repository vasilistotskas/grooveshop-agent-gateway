package acp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func shippingFeeMinor(o django.QuoteShippingOption) (int64, bool) {
	fee, err := money.MinorUnits(o.Price.String())
	return fee, err == nil
}

// discountExtension is the capabilities.extensions declaration for the ACP
// discount extension (URLs per the spec's own examples).
var discountExtension = ExtensionDeclaration{
	Name: "discount",
	Extends: []string{
		"$.CheckoutSessionCreateRequest.discounts",
		"$.CheckoutSessionUpdateRequest.discounts",
		"$.CheckoutSession.discounts",
	},
	Schema: "https://agenticcommerce.dev/schemas/discount.json",
	Spec:   "https://agenticcommerce.dev/specs/discount",
}

// capabilitiesOf is what a store's sessions declare: the discount
// extension only while it has promotions on, since otherwise every code
// is refused.
func capabilitiesOf(t *tenant.Tenant) Capabilities {
	if !t.Features.Promotions {
		return Capabilities{}
	}
	return Capabilities{Extensions: []ExtensionDeclaration{discountExtension}}
}

// statusOf maps the shared checkout lifecycle onto ACP's vocabulary.
// ready_for_complete renders as ready_for_payment only when an offline
// (agent-completable) pay way exists — otherwise the honest state is
// requires_escalation with continue_url for the browser handoff.
func statusOf(s *checkout.Session, offlinePayWay bool) string {
	switch s.Status {
	case checkout.StatusIncomplete:
		return "not_ready_for_payment"
	case checkout.StatusReadyForComplete:
		if offlinePayWay {
			return "ready_for_payment"
		}
		return "requires_escalation"
	default:
		return string(s.Status)
	}
}

// Render serializes the shared session into the ACP response payload.
func Render(
	ctx context.Context, dj *django.Client, t *tenant.Tenant,
	links storefront.Links, s *checkout.Session,
) (*Session, error) {
	pricing, _, err := checkout.ComputePricing(ctx, dj, t, s)
	if err != nil {
		return nil, fmt.Errorf("acp: pricing: %w", err)
	}
	// The total this render publishes is the one the order is placed
	// at; the handler stores it (Handler.renderBody).
	s.Show(pricing)

	out := &Session{
		ID:       s.ID,
		Protocol: Protocol{Version: Version},
		// The discount extension is declared only by a store that takes
		// discount codes (promotions on).
		Capabilities:       capabilitiesOf(t),
		Currency:           t.DefaultCurrency,
		LineItems:          []LineItem{},
		FulfillmentOptions: []FulfillmentOption{},
		Totals:             []Total{},
		Messages:           []Message{},
		Links: []Link{
			{Type: "terms_of_use", URL: links.Terms()},
			{Type: "privacy_policy", URL: links.Privacy()},
		},
		// Handoff and session recovery: the buyer claims this cart in the
		// browser and finishes checkout there (card payment included).
		ContinueURL: links.CartClaim(s.CartID),
	}
	if !s.ExpiresAt.IsZero() {
		out.ExpiresAt = s.ExpiresAt.UTC().Format(time.RFC3339)
	}
	switch {
	// A placed order has no cart left to claim: the buyer goes to the order.
	case s.OrderUUID != "":
		out.ContinueURL = links.OrderSuccess(s.OrderUUID)
	// An order that is being placed, or was never confirmed, may exist:
	// claiming the cart would start a second purchase, so no link.
	case s.MayHaveOrder():
		out.ContinueURL = ""
	}

	if s.Buyer != (checkout.Buyer{}) {
		out.Buyer = &Buyer{
			FirstName:   s.Buyer.FirstName,
			LastName:    s.Buyer.LastName,
			Email:       s.Buyer.Email,
			PhoneNumber: s.Buyer.Phone,
		}
	}

	lineIDs := make([]string, 0, len(pricing.Lines))
	for _, line := range pricing.Lines {
		id := strconv.FormatInt(line.LineID, 10)
		lineIDs = append(lineIDs, id)
		out.LineItems = append(out.LineItems, LineItem{
			ID: id,
			Item: Item{
				ID:         strconv.FormatInt(line.ProductID, 10),
				Name:       line.Title,
				UnitAmount: line.UnitMinor,
			},
			Quantity: line.Quantity,
			Name:     line.Title,
			Totals: []Total{
				{Type: "subtotal", DisplayText: "Subtotal",
					Amount: line.TotalMinor},
				{Type: "total", DisplayText: "Total",
					Amount: line.TotalMinor},
			},
		})
	}

	renderFulfillment(s, pricing, out, lineIDs)

	out.Totals = append(out.Totals, Total{
		Type: "subtotal", DisplayText: "Subtotal",
		Amount: pricing.ItemsSubtotal,
	})
	if pricing.DiscountTotal > 0 {
		// ACP totals carry the discount as a positive amount under its
		// own type (the applied_discount amounts are non-negative too);
		// the total row is already discount-aware.
		out.Totals = append(out.Totals, Total{
			Type: "discount", DisplayText: "Discount",
			Amount: pricing.DiscountTotal,
		})
	}
	if pricing.HasDelivery {
		out.Totals = append(out.Totals, Total{
			Type: "fulfillment", DisplayText: "Shipping",
			Amount: pricing.DeliveryFee,
		})
	}
	if pricing.HasPaymentFee {
		out.Totals = append(out.Totals, Total{
			Type: "fee", DisplayText: "Payment method fee",
			Amount: pricing.PaymentFee,
		})
	}
	out.Totals = append(out.Totals, Total{
		Type: "total", DisplayText: "Total", Amount: pricing.Total,
	})

	renderDiscounts(s, pricing, out)

	// An ACP session carries a pay way only when the store offers a
	// collect-later method for its delivery (Handler.selectPayWay).
	out.Status = statusOf(s, s.PayWayID > 0)

	appendStateMessages(s, out)
	if refusals := refusedChoices(pricing); len(refusals) > 0 && s.Open() {
		// A choice the store no longer takes is left out of the price,
		// so the session cannot be paid until it is chosen again.
		out.Status = "not_ready_for_payment"
		out.Messages = append(out.Messages, refusals...)
	}

	if s.Status == checkout.StatusCompleted && s.OrderUUID != "" {
		out.Order = &Order{
			ID:                s.OrderUUID,
			CheckoutSessionID: s.ID,
			OrderNumber:       strconv.FormatInt(s.OrderID, 10),
			PermalinkURL:      links.OrderSuccess(s.OrderUUID),
		}
	}
	return out, nil
}

// renderFulfillment fills details, available options and the selection.
// Options list home-delivery methods for the buyer's country, priced by
// the quote as an order would be — pickup points need station-level
// choices ACP cannot express; those buyers hand off via continue_url.
func renderFulfillment(
	s *checkout.Session, pricing *checkout.Pricing, out *Session,
	lineIDs []string,
) {
	f := s.Fulfillment
	if f.Street != "" || f.City != "" || f.Zipcode != "" ||
		f.CountryCode != "" {
		name := strings.TrimSpace(s.Buyer.FirstName + " " + s.Buyer.LastName)
		line := f.Street
		if f.StreetNumber != "" {
			line += " " + f.StreetNumber
		}
		out.FulfillmentDetails = &FulfillmentDetails{
			Name:        name,
			PhoneNumber: s.Buyer.Phone,
			Email:       s.Buyer.Email,
			Address: &Address{
				Name:       name,
				LineOne:    line,
				City:       f.City,
				State:      f.RegionID,
				Country:    f.CountryCode,
				PostalCode: f.Zipcode,
			},
		}
	}

	// Options are for a buyer still choosing: a placed order has none.
	offered := map[string]bool{}
	for _, o := range pricing.ShippingOptions {
		// Over the rate's weight cap, or too large for the carrier's
		// parcels, Django refuses the order, so the agent must never be
		// offered it.
		if o.Kind != checkout.FulfillmentHomeDelivery || !o.FitsCart() {
			continue
		}
		fee, ok := shippingFeeMinor(o)
		if !ok {
			continue
		}
		offered[o.ProviderCode+":"+o.Kind] = true
		out.FulfillmentOptions = append(out.FulfillmentOptions,
			FulfillmentOption{
				Type:  "shipping",
				ID:    o.ProviderCode + ":" + o.Kind,
				Title: o.ProviderName,
				Totals: []Total{{
					Type: "total", DisplayText: "Shipping", Amount: fee,
				}},
			})
	}

	// A selection whose option was filtered out (the cart outgrew its
	// weight cap) is not echoed: the buyer could not choose it again.
	selected := f.ProviderCode + ":" + f.Kind
	if f.ProviderCode != "" && f.Kind == checkout.FulfillmentHomeDelivery &&
		offered[selected] {
		out.SelectedFulfillmentOptions = []SelectedFulfillmentOption{{
			Type:     "shipping",
			OptionID: selected,
			ItemIDs:  lineIDs,
		}}
	}
}

// renderDiscounts fills the session's discount-extension block: submitted
// codes echo from the gateway session; applied discounts are Django's
// per-offer breakdown, each with its own amount (a coupon is never
// credited with an automatic promotion's money); rejections are the last
// submission's plus the attached codes Django refuses now, and surface
// both in discounts.rejected and as warning messages (MessageWarning
// carries the discount_code_* vocabulary).
func renderDiscounts(
	s *checkout.Session, pricing *checkout.Pricing, out *Session,
) {
	d := &Discounts{Codes: s.DiscountCodes}
	for _, disc := range pricing.Discounts {
		if disc.Code == "" {
			id := "promotion_" + strconv.FormatInt(disc.PromotionID, 10)
			d.Applied = append(d.Applied, AppliedDiscount{
				ID:        id,
				Coupon:    Coupon{ID: id, Name: disc.Name},
				Amount:    disc.Amount,
				Automatic: true,
			})
			continue
		}
		d.Applied = append(d.Applied, AppliedDiscount{
			ID:     disc.Code,
			Code:   disc.Code,
			Coupon: Coupon{ID: disc.Code, Name: disc.Name},
			Amount: disc.Amount,
		})
	}
	for _, rej := range checkout.Rejections(s, pricing) {
		d.Rejected = append(d.Rejected, RejectedDiscount{
			Code: rej.Code, Reason: rej.Reason, Message: rej.Message,
		})
		content := rej.Message
		if content == "" {
			content = fmt.Sprintf(
				"Discount code %q could not be applied.", rej.Code)
		}
		out.Messages = append(out.Messages, Message{
			Type: "warning", Code: rej.Reason,
			Param: "$.discounts.codes", Resolution: "recoverable",
			ContentType: "plain", Content: content,
		})
	}
	if len(d.Codes)+len(d.Applied)+len(d.Rejected) > 0 {
		out.Discounts = d
	}
}

// appendStateMessages surfaces what still blocks payment, in the spec's
// error vocabulary so agents know what buyer input to collect.
// refusedChoices are the errors of the session's choices the store
// refused for this cart, in the store's words: ACP's invalid, at the
// member the buyer chooses again.
func refusedChoices(p *checkout.Pricing) []Message {
	var out []Message
	if r := p.DeliveryRefused; r != nil {
		out = append(out, Message{
			Type: "error", Code: "invalid",
			Param:      "$.selected_fulfillment_options",
			Resolution: "requires_buyer_input", ContentType: "plain",
			Content: strings.TrimSpace(r.Detail + " Select another " +
				"fulfillment option from fulfillment_options."),
		})
	}
	if r := p.PayWayRefused; r != nil {
		out = append(out, Message{
			Type: "error", Code: "invalid", Param: "$.payment_provider",
			Resolution: "requires_buyer_input", ContentType: "plain",
			Content: strings.TrimSpace(r.Detail + " The store offers no " +
				"collect-later payment for this delivery; select another " +
				"fulfillment option."),
		})
	}
	if r := p.InputRefused; r != nil {
		out = append(out, Message{
			Type: "error", Code: "invalid",
			Resolution: "requires_buyer_input", ContentType: "plain",
			Content: strings.TrimSpace("The store refuses this session's " +
				"data: " + r.Detail + " Only its items are priced: " +
				"correct it with an update."),
		})
	}
	return out
}

func appendStateMessages(s *checkout.Session, out *Session) {
	if s.Status != checkout.StatusIncomplete {
		if out.Status == "requires_escalation" {
			out.Messages = append(out.Messages, Message{
				Type: "info", ContentType: "plain",
				Resolution: "requires_buyer_review",
				Content: "Payment must be completed by the buyer in the " +
					"browser — send them to continue_url.",
			})
		}
		return
	}
	missing := func(param, content string) {
		out.Messages = append(out.Messages, Message{
			Type: "error", Code: "missing", Param: param,
			Resolution: "requires_buyer_input", ContentType: "plain",
			Content: content,
		})
	}
	if !s.Buyer.Complete() {
		missing("$.buyer",
			"Buyer first name, last name, email and phone are required.")
	}
	f := s.Fulfillment
	if f.CountryCode == "" || f.City == "" || f.Zipcode == "" ||
		f.Street == "" {
		missing("$.fulfillment_details.address",
			"A delivery address (line_one, city, postal_code, country) "+
				"is required.")
	} else if f.StreetNumber == "" {
		// ACP has no house-number field; splitStreetNumber reads it off
		// the end of line_one.
		missing("$.fulfillment_details.address.line_one",
			"line_one must end with the house number, e.g. "+
				"\"Main Street 12\".")
	}
	if choices := s.RegionChoices(); choices != "" {
		missing("$.fulfillment_details.address.state",
			"state must name a region of the country, one of: "+choices)
	}
	if f.ProviderCode == "" || f.Kind == "" {
		missing("$.selected_fulfillment_options",
			"Select a fulfillment option from fulfillment_options.")
	}
}
