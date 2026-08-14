package acp

import (
	"context"
	"fmt"
	"strings"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func shippingFeeMinor(o django.ShippingOption) (int64, bool) {
	fee, err := money.MinorUnits(o.Price.String())
	return fee, err == nil
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

// offlinePayWay returns the first active offline payment method — the one
// agentic completion uses until tokenized card payment ships.
func offlinePayWay(
	ctx context.Context, dj *django.Client, t *tenant.Tenant,
) (*django.PayWay, error) {
	page, err := dj.PayWays(ctx, t.Domain, t.DefaultLocale, "", "")
	if err != nil {
		return nil, err
	}
	for i := range page.Results {
		pw := &page.Results[i]
		if pw.Active && !pw.IsOnlinePayment {
			return pw, nil
		}
	}
	return nil, nil
}

// Render serializes the shared session into the ACP response payload.
func Render(
	ctx context.Context, dj *django.Client, t *tenant.Tenant,
	s *checkout.Session,
) (*Session, error) {
	pricing, cart, err := checkout.ComputePricing(ctx, dj, t, s)
	if err != nil {
		return nil, fmt.Errorf("acp: pricing: %w", err)
	}

	out := &Session{
		ID:                 s.ID,
		Protocol:           Protocol{Version: Version},
		Capabilities:       Capabilities{},
		Currency:           t.DefaultCurrency,
		LineItems:          []LineItem{},
		FulfillmentOptions: []FulfillmentOption{},
		Totals:             []Total{},
		Messages:           []Message{},
		Links: []Link{
			{Type: "terms_of_use",
				URL: fmt.Sprintf("https://%s/terms-and-conditions", t.Domain)},
			{Type: "privacy_policy",
				URL: fmt.Sprintf("https://%s/privacy-policy", t.Domain)},
		},
		// Handoff and session recovery: the buyer claims this cart in the
		// browser and finishes checkout there (card payment included).
		ContinueURL: fmt.Sprintf("https://%s/cart/claim?uuid=%s",
			t.Domain, s.CartID),
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
		id := fmt.Sprintf("%d", line.CartItemID)
		lineIDs = append(lineIDs, id)
		out.LineItems = append(out.LineItems, LineItem{
			ID: id,
			Item: Item{
				ID:         fmt.Sprintf("%d", line.ProductID),
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

	renderFulfillment(ctx, dj, t, s, cart, out, lineIDs)

	out.Totals = append(out.Totals, Total{
		Type: "subtotal", DisplayText: "Subtotal",
		Amount: pricing.ItemsSubtotal,
	})
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

	offline := false
	if s.Status == checkout.StatusReadyForComplete {
		pw, err := offlinePayWay(ctx, dj, t)
		if err != nil {
			return nil, fmt.Errorf("acp: pay ways: %w", err)
		}
		offline = pw != nil
	}
	out.Status = statusOf(s, offline)

	appendStateMessages(s, out)

	if s.Status == checkout.StatusCompleted && s.OrderUUID != "" {
		out.Order = &Order{
			ID:                s.OrderUUID,
			CheckoutSessionID: s.ID,
			OrderNumber:       fmt.Sprintf("%d", s.OrderID),
			PermalinkURL: fmt.Sprintf("https://%s/checkout/success/%s",
				t.Domain, s.OrderUUID),
		}
	}
	return out, nil
}

// renderFulfillment fills details, available options and the selection.
// Options list home-delivery methods for the buyer's country — pickup
// points need station-level choices ACP cannot express; those buyers
// hand off via continue_url.
func renderFulfillment(
	ctx context.Context, dj *django.Client, t *tenant.Tenant,
	s *checkout.Session, cart *django.Cart, out *Session, lineIDs []string,
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
				State:      "",
				Country:    f.CountryCode,
				PostalCode: f.Zipcode,
			},
		}
	}

	if f.CountryCode == "" {
		return
	}
	opts, err := dj.ShippingOptions(ctx, t.Domain, t.DefaultLocale,
		django.ShippingQuery{
			CountryCode:      f.CountryCode,
			OrderValueAmount: cart.TotalPrice.String(),
			Currency:         t.DefaultCurrency,
		})
	if err != nil {
		return // advisory: options re-render on the next call
	}
	for _, o := range opts {
		if o.Kind != checkout.FulfillmentHomeDelivery {
			continue
		}
		fee, ok := shippingFeeMinor(o)
		if !ok {
			continue
		}
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

	if f.ProviderCode != "" && f.Kind == checkout.FulfillmentHomeDelivery {
		out.SelectedFulfillmentOptions = []SelectedFulfillmentOption{{
			Type:     "shipping",
			OptionID: f.ProviderCode + ":" + f.Kind,
			ItemIDs:  lineIDs,
		}}
	}
}

// appendStateMessages surfaces what still blocks payment, in the spec's
// error vocabulary so agents know what buyer input to collect.
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
	}
	if f.ProviderCode == "" || f.Kind == "" {
		missing("$.selected_fulfillment_options",
			"Select a fulfillment option from fulfillment_options.")
	}
}
