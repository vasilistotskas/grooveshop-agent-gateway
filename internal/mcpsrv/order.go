package mcpsrv

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

type TrackOrderIn struct {
	OrderUUID string `json:"orderUuid" jsonschema:"the order's UUID from the confirmation email or checkout success page"`
}

// TrackOrderItem is one order line as it was sold: the name and SKU it was
// sold under, in the order's language, not the product's current ones.
type TrackOrderItem struct {
	Name     string `json:"name"`
	SKU      string `json:"sku,omitempty"`
	Quantity int    `json:"quantity"`
	Total    string `json:"total"`
}

type TrackOrderOut struct {
	OrderUUID     string           `json:"orderUuid"`
	Status        string           `json:"status"`
	StatusDisplay string           `json:"statusDisplay"`
	PaymentStatus string           `json:"paymentStatus"`
	IsPaid        bool             `json:"isPaid"`
	PlacedAt      string           `json:"placedAt"`
	Items         []TrackOrderItem `json:"items"`
	GrandTotal    string           `json:"grandTotal"`
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

// orderNotOpened answers a uuid that opens no order. Django refuses a guest's
// order once its uuid has expired, so the order may well exist; the same words
// for an unknown uuid keep that from telling a caller which one it holds.
const orderNotOpened = "no order can be opened with that id: it may be " +
	"mistyped, or too old to open without signing in; check it against the " +
	"confirmation email, then have the shopper sign in or contact the store"

func (h *handlers) trackOrder(
	ctx context.Context, _ *mcp.CallToolRequest, in TrackOrderIn,
) (*mcp.CallToolResult, TrackOrderOut, error) {
	var out TrackOrderOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	lang := django.Language(ctx, t.TenantConfig)
	if in.OrderUUID == "" {
		return nil, out, errors.New("orderUuid is required")
	}

	o, err := h.deps.Django.OrderByUUID(ctx, t.Domain, lang, in.OrderUUID)
	if err != nil {
		return nil, out, upstreamErr(err, orderNotOpened)
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
	out.Items = make([]TrackOrderItem, 0, len(o.Items))
	for _, it := range o.Items {
		out.Items = append(out.Items, TrackOrderItem{
			Name:     it.ProductName,
			SKU:      it.SKU,
			Quantity: it.Quantity,
			Total:    num(it.TotalPrice),
		})
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

// GetOrderIn is the get_order tool's arguments.
type GetOrderIn struct {
	Meta *MetaIn `json:"meta"`
	ID   string  `json:"id" jsonschema:"the order id from the completed checkout"`
}

// getOrder renders the canonical UCP order object.
//
// The order's `checkout_id` comes from the gateway's own order index
// rather than from the order payload: nothing upstream records which
// agent session placed an order, and the schema requires the link for
// reconciliation. An order placed outside this gateway — on the web, say
// — therefore has no checkout to name, and saying so is better than
// inventing one.
func (h *handlers) getOrder(
	ctx context.Context, _ *mcp.CallToolRequest, in GetOrderIn,
) (*mcp.CallToolResult, OrderResult, error) {
	var zero OrderResult
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	lang := django.Language(ctx, t.TenantConfig)
	if err := in.Meta.validate(false); err != nil {
		return nil, zero, err
	}
	neg, err := h.negotiate(ctx, t, in.Meta, h.linksFor(ctx, t).Home())
	if err != nil {
		return nil, zero, err
	}
	if !neg.Has(ucp.CapabilityOrder) {
		return nil, OrderResult{failure: ucp.CapabilitiesIncompatible(
			h.linksFor(ctx, t).Home())}, nil
	}
	if in.ID == "" {
		return nil, zero, errors.New("id is required")
	}

	order, err := h.deps.Django.OrderByUUID(
		ctx, t.Domain, lang, in.ID)
	if err != nil {
		return nil, zero, upstreamErr(err, orderNotOpened)
	}

	link, err := h.deps.Checkout.OrderLinkFor(ctx, t.SchemaName, in.ID)
	if err != nil {
		if errors.Is(err, checkout.ErrNotFound) {
			// No link says only that none is held: the order was placed
			// elsewhere, or here and its link is gone (expired, or
			// evicted). The order itself was just read.
			return nil, zero, fmt.Errorf(
				"order %s exists, but no checkout is held for it here: "+
					"it was not placed through this agent surface, or it "+
					"was and its checkout record is no longer kept. "+
					"get_order needs that record; use track_order, which "+
					"reads the order's status from the store by its id",
				in.ID)
		}
		return nil, zero, errCheckoutUnavailable
	}

	out, err := ucp.BuildOrder(t, h.linksFor(ctx, t), order,
		link.CheckoutID,
		neg.Declaration(ucp.CapabilityOrder))
	if err != nil {
		return nil, zero, err
	}
	return nil, OrderResult{order: out}, nil
}
