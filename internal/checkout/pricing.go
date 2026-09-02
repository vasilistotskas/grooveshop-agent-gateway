package checkout

import (
	"context"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// PricedLine is one cart line with amounts in ISO 4217 minor units.
type PricedLine struct {
	CartItemID int64
	ProductID  int64
	Title      string
	ImagePath  string
	Quantity   int
	UnitMinor  int64
	TotalMinor int64
}

// Pricing is the protocol-neutral totals breakdown both the UCP and ACP
// renderers serialize. Amounts are minor units; fees are present only when
// the session has selected the option they derive from.
//
// DiscountTotal is the promotion/coupon discount Django evaluated on the
// cart — it reduces Total. MarkdownTotal is the product-markdown savings
// already baked into the line prices (informational only: emitting it as
// a totals row would double-count). FreeShipping zeroes the delivery fee.
type Pricing struct {
	Lines         []PricedLine
	ItemsSubtotal int64
	DiscountTotal int64
	MarkdownTotal int64
	FreeShipping  bool
	DeliveryFee   int64
	HasDelivery   bool
	PaymentFee    int64
	HasPaymentFee bool
	Total         int64
	// PayWay is the pay-way the buyer selected, or nil when none is
	// selected yet or the lookup failed. Callers deciding whether an
	// agent can settle a checkout unaided MUST read this rather than the
	// store's advertised set: the selection is the authoritative fact
	// for THIS checkout.
	PayWay *django.PayWay
}

// ComputePricing fetches the cart fresh and derives the totals breakdown
// (items + selected delivery + selected pay-way fee). Money totals are
// never stored on the session — Django stays authoritative.
func ComputePricing(
	ctx context.Context, dj *django.Client, t *tenant.Tenant, s *Session,
) (*Pricing, *django.Cart, error) {
	cart, err := dj.GetCart(ctx, t.Domain, t.DefaultLocale, s.CartID)
	if err != nil {
		return nil, nil, err
	}

	p := &Pricing{Lines: []PricedLine{}}
	for _, it := range cart.Items {
		tr := django.Localized(it.Product.Translations, t.DefaultLocale)
		unit, err := money.MinorUnits(it.FinalPrice.String())
		if err != nil {
			return nil, nil, err
		}
		lineTotal, err := money.MinorUnits(it.TotalPrice.String())
		if err != nil {
			return nil, nil, err
		}
		p.ItemsSubtotal += lineTotal
		p.Lines = append(p.Lines, PricedLine{
			CartItemID: it.ID,
			ProductID:  it.Product.ID,
			Title:      tr.Name,
			ImagePath:  it.Product.MainImagePath,
			Quantity:   it.Quantity,
			UnitMinor:  unit,
			TotalMinor: lineTotal,
		})
	}
	p.DiscountTotal, err = money.MinorUnits(cart.PromotionDiscount.String())
	if err != nil {
		return nil, nil, err
	}
	p.MarkdownTotal, err = money.MinorUnits(cart.TotalDiscountValue.String())
	if err != nil {
		return nil, nil, err
	}
	p.FreeShipping = cart.PromotionFreeShipping
	p.Total = p.ItemsSubtotal - p.DiscountTotal

	if fee, ok := deliveryFee(ctx, dj, t, s, cart); ok {
		// A promotion-granted free shipping keeps the fulfillment row
		// (the option stays selected) but zeroes its cost.
		if p.FreeShipping {
			fee = 0
		}
		p.DeliveryFee, p.HasDelivery = fee, true
		p.Total += fee
	}
	if s.PayWayID > 0 {
		// One lookup serves both the fee and the settlement question.
		if pw, err := dj.PayWayByID(
			ctx, t.Domain, t.DefaultLocale, s.PayWayID,
		); err == nil {
			p.PayWay = pw
		}
	}
	if fee, ok := paymentFee(p.PayWay, cart); ok {
		p.PaymentFee, p.HasPaymentFee = fee, true
		p.Total += fee
	}
	if p.Total < 0 {
		p.Total = 0
	}
	return p, cart, nil
}

// deliveryFee resolves the chosen shipping option's price. Fee lookups are
// advisory (totals render without them on upstream failure); order creation
// recomputes authoritatively in Django.
func deliveryFee(
	ctx context.Context, dj *django.Client, t *tenant.Tenant,
	s *Session, cart *django.Cart,
) (int64, bool) {
	f := s.Fulfillment
	if f.ProviderCode == "" || f.Kind == "" || f.CountryCode == "" {
		return 0, false
	}
	opts, err := dj.ShippingOptions(ctx, t.Domain, t.DefaultLocale,
		django.ShippingQuery{
			CountryCode:      f.CountryCode,
			OrderValueAmount: cart.TotalPrice.String(),
			Currency:         t.DefaultCurrency,
		})
	if err != nil {
		return 0, false
	}
	for _, o := range opts {
		if o.ProviderCode == f.ProviderCode && o.Kind == f.Kind {
			fee, err := money.MinorUnits(o.Price.String())
			return fee, err == nil
		}
	}
	return 0, false
}

// paymentFee resolves the chosen pay way's fee, waived above its free
// threshold.
func paymentFee(
	pw *django.PayWay, cart *django.Cart,
) (int64, bool) {
	if pw == nil {
		return 0, false
	}
	cost, err := money.MinorUnits(pw.Cost.String())
	if err != nil {
		return 0, false
	}
	threshold, err := money.MinorUnits(pw.FreeThreshold.String())
	if err != nil {
		return 0, false
	}
	cartTotal, err := money.MinorUnits(cart.TotalPrice.String())
	if err != nil {
		return 0, false
	}
	if cost == 0 || (threshold > 0 && cartTotal >= threshold) {
		return 0, false
	}
	return cost, true
}
