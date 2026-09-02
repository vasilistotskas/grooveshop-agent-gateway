package ucp

import (
	"fmt"
	"strconv"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Order is the dev.ucp.shopping.order capability's response object,
// mirroring shopping/order.json.
type Order struct {
	UCP          OrderEnvelope    `json:"ucp"`
	ID           string           `json:"id"`
	Label        string           `json:"label,omitempty"`
	CheckoutID   string           `json:"checkout_id"`
	PermalinkURL string           `json:"permalink_url"`
	LineItems    []OrderLineItem  `json:"line_items"`
	Fulfillment  OrderFulfillment `json:"fulfillment"`
	Currency     string           `json:"currency"`
	Totals       []LineItemTotal  `json:"totals"`
	Messages     []Message        `json:"messages,omitempty"`
}

// OrderEnvelope is the order response's protocol metadata. The order
// response schema carries no payment handlers: nothing is left to pay.
type OrderEnvelope struct {
	Version string `json:"version"`
}

// OrderLineItem is one purchased line. `status` is DERIVED from
// quantities, not stored: fulfilled when all units shipped, partial when
// some did, removed at zero, processing otherwise.
type OrderLineItem struct {
	ID       string          `json:"id"`
	Item     Item            `json:"item"`
	Quantity OrderQuantity   `json:"quantity"`
	Totals   []LineItemTotal `json:"totals"`
	Status   string          `json:"status"`
}

// OrderQuantity splits a line's units by fulfilment state.
type OrderQuantity struct {
	Total     int `json:"total"`
	Fulfilled int `json:"fulfilled"`
}

// OrderFulfillment holds buyer expectations and the shipment log.
//
// Both members are optional and both are currently omitted. An
// expectation requires a `destination`, and the order detail this
// gateway reads deliberately does not decode the recipient's address —
// possession of an order UUID authorises status tracking, not PII
// retrieval. An event requires `occurred_at`, and the upstream tracking
// payload carries no shipment timestamp. Emitting either would mean
// inventing data, so the object stays empty until the upstream supplies
// what the schema requires.
type OrderFulfillment struct{}

// BuildOrder renders a Django order as the UCP order object.
//
// checkoutID is the session that produced the order. The schema requires
// it for reconciliation, so a caller that cannot supply it must surface
// that rather than pass an empty string.
func BuildOrder(
	t *tenant.Tenant, o *django.Order, checkoutID string,
) (*Order, error) {
	if checkoutID == "" {
		return nil, fmt.Errorf(
			"ucp: order %s has no known checkout session", o.UUID)
	}

	currency := o.PricingBreakdown.Currency
	if currency == "" {
		currency = t.DefaultCurrency
	}

	out := &Order{
		UCP:          OrderEnvelope{Version: Version},
		ID:           o.UUID,
		Label:        fmt.Sprintf("Order %d", o.ID),
		CheckoutID:   checkoutID,
		PermalinkURL: storefront.OrderSuccess(t.Domain, o.UUID),
		LineItems:    make([]OrderLineItem, 0, len(o.Items)),
		Currency:     currency,
	}

	// Fulfilment is tracked per shipment upstream, not per line, so a
	// line reads as fulfilled once the order itself has shipped.
	fulfilled := o.TrackingDetails != nil && o.TrackingDetails.HasTracking

	for i, item := range o.Items {
		lineTotal, err := money.MinorUnits(item.TotalPrice.String())
		if err != nil {
			return nil, fmt.Errorf("ucp: order line %d total: %w", i, err)
		}
		unit, err := money.MinorUnits(item.Price.String())
		if err != nil {
			return nil, fmt.Errorf("ucp: order line %d price: %w", i, err)
		}

		shipped := 0
		if fulfilled {
			shipped = item.Quantity
		}
		out.LineItems = append(out.LineItems, OrderLineItem{
			ID: strconv.FormatInt(item.Product.ID, 10),
			Item: Item{
				ID: strconv.FormatInt(item.Product.ID, 10),
				Title: django.Localized(
					item.Product.Translations, t.DefaultLocale).Name,
				Price: unit,
			},
			Quantity: OrderQuantity{
				Total: item.Quantity, Fulfilled: shipped,
			},
			Totals: []LineItemTotal{
				{Type: "subtotal", Amount: lineTotal},
				{Type: "total", Amount: lineTotal},
			},
			Status: orderLineStatus(item.Quantity, shipped),
		})
	}

	totals, err := orderTotals(o)
	if err != nil {
		return nil, err
	}
	out.Totals = totals
	return out, nil
}

// orderLineStatus derives the line's status from its quantities, exactly
// as the schema defines it.
func orderLineStatus(total, fulfilled int) string {
	switch {
	case total == 0:
		return "removed"
	case fulfilled > 0 && fulfilled == total:
		return "fulfilled"
	case fulfilled > 0:
		return "partial"
	default:
		return "processing"
	}
}

// orderTotals renders the pricing breakdown. The schema requires exactly
// one subtotal and one total; detail lines appear only when non-zero, so
// a platform never renders a meaningless "€0.00 discount" row.
func orderTotals(o *django.Order) ([]LineItemTotal, error) {
	p := o.PricingBreakdown
	amount := func(label string, n string) (int64, error) {
		v, err := money.MinorUnits(n)
		if err != nil {
			return 0, fmt.Errorf("ucp: order %s: %w", label, err)
		}
		return v, nil
	}

	subtotal, err := amount("itemsSubtotal", p.ItemsSubtotal.String())
	if err != nil {
		return nil, err
	}
	totals := []LineItemTotal{{Type: "subtotal", Amount: subtotal}}

	shipping, err := amount("shippingCost", p.ShippingCost.String())
	if err != nil {
		return nil, err
	}
	if shipping > 0 {
		totals = append(totals, LineItemTotal{
			Type: "fulfillment", Amount: shipping})
	}

	fee, err := amount("paymentMethodFee", p.PaymentMethodFee.String())
	if err != nil {
		return nil, err
	}
	if fee > 0 {
		totals = append(totals, LineItemTotal{
			Type: "fee", DisplayText: "Payment method fee", Amount: fee})
	}

	// UCP types discount amounts as strictly negative.
	for _, d := range []struct {
		label, text string
		raw         string
	}{
		{"discount", "Discount", p.Discount.String()},
		{"loyaltyDiscount", "Loyalty discount", p.LoyaltyDiscount.String()},
		{"giftCardAmount", "Gift card", p.GiftCardAmount.String()},
	} {
		v, err := amount(d.label, d.raw)
		if err != nil {
			return nil, err
		}
		if v > 0 {
			totals = append(totals, LineItemTotal{
				Type: "discount", DisplayText: d.text, Amount: -v})
		}
	}

	grand, err := amount("grandTotal", p.GrandTotal.String())
	if err != nil {
		return nil, err
	}
	return append(totals,
		LineItemTotal{Type: "total", Amount: grand}), nil
}
