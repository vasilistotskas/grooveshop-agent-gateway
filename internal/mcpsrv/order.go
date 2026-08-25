package mcpsrv

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type TrackOrderIn struct {
	OrderUUID string `json:"orderUuid" jsonschema:"the order's UUID from the confirmation email or checkout success page"`
}

type TrackOrderOut struct {
	OrderUUID     string `json:"orderUuid"`
	Status        string `json:"status"`
	StatusDisplay string `json:"statusDisplay"`
	PaymentStatus string `json:"paymentStatus"`
	IsPaid        bool   `json:"isPaid"`
	PlacedAt      string `json:"placedAt"`
	Items         []struct {
		Name     string `json:"name"`
		Quantity int    `json:"quantity"`
		Total    string `json:"total"`
	} `json:"items"`
	GrandTotal string `json:"grandTotal"`
	// Discount fields mirror the order's pricingBreakdown and are
	// present only when non-zero.
	Discount        string `json:"discount,omitempty" jsonschema:"promotion/coupon discount included in grandTotal"`
	LoyaltyDiscount string `json:"loyaltyDiscount,omitempty"`
	GiftCardAmount  string `json:"giftCardAmount,omitempty" jsonschema:"amount covered by gift cards"`
	Currency        string `json:"currency"`
	Tracking        *struct {
		Carrier string `json:"carrier"`
		Number  string `json:"number"`
		URL     string `json:"url"`
	} `json:"tracking,omitempty"`
}

func (h *handlers) trackOrder(
	ctx context.Context, _ *mcp.CallToolRequest, in TrackOrderIn,
) (*mcp.CallToolResult, TrackOrderOut, error) {
	var out TrackOrderOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	if in.OrderUUID == "" {
		return nil, out, errors.New("orderUuid is required")
	}

	o, err := h.deps.Django.OrderByUUID(ctx, t.Domain, t.DefaultLocale, in.OrderUUID)
	if err != nil {
		return nil, out, upstreamErr(err,
			"no order was found for that UUID; double-check it against "+
				"the confirmation email")
	}

	out.OrderUUID = o.UUID
	out.Status = o.Status
	out.StatusDisplay = o.StatusDisplay
	out.PaymentStatus = o.PaymentStatusDisplay
	out.IsPaid = o.IsPaid
	out.PlacedAt = o.CreatedAt
	out.GrandTotal = num(o.PricingBreakdown.GrandTotal)
	out.Discount = posNum(o.PricingBreakdown.Discount)
	out.LoyaltyDiscount = posNum(o.PricingBreakdown.LoyaltyDiscount)
	out.GiftCardAmount = posNum(o.PricingBreakdown.GiftCardAmount)
	out.Currency = o.PricingBreakdown.Currency
	for _, it := range o.Items {
		tr := localized(it.Product.Translations, t.DefaultLocale)
		out.Items = append(out.Items, struct {
			Name     string `json:"name"`
			Quantity int    `json:"quantity"`
			Total    string `json:"total"`
		}{tr.Name, it.Quantity, num(it.TotalPrice)})
	}
	if td := o.TrackingDetails; td != nil && td.HasTracking {
		out.Tracking = &struct {
			Carrier string `json:"carrier"`
			Number  string `json:"number"`
			URL     string `json:"url"`
		}{td.ShippingCarrier, td.TrackingNumber, td.TrackingURL}
	}

	trackNote := "No tracking number yet."
	if out.Tracking != nil {
		trackNote = "Track it at " + out.Tracking.URL
	}
	return textResult(
		"Order %s: %s, payment %s. Total %s %s. %s",
		out.OrderUUID, out.StatusDisplay, out.PaymentStatus,
		out.GrandTotal, out.Currency, trackNote,
	), out, nil
}
