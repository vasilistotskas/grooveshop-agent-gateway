package checkout

import (
	"context"
	"errors"
	"fmt"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// PricedLine is one line with amounts in ISO 4217 minor units. LineID is
// its cart item, or, once the order is placed, its product, as get_order
// names the line.
type PricedLine struct {
	LineID     int64
	ProductID  int64
	Title      string
	ImagePath  string
	Quantity   int
	UnitMinor  int64
	TotalMinor int64
}

// PricedDiscount is one discount on the cart: an offer's share of
// DiscountTotal (Code empty for an automatic promotion), or an attached
// coupon whose benefit is not money — free shipping or a gift — at 0.
type PricedDiscount struct {
	PromotionID int64
	Name        string
	Code        string
	Amount      int64
}

// Pricing is the protocol-neutral totals breakdown both the UCP and ACP
// renderers serialize. Amounts are minor units. Before an order exists
// every amount is Django's checkout quote (django.Client.Quote), the one
// computation order creation runs; the gateway adds nothing up of its
// own except the total of a checkout that has not chosen everything yet:
// the rows the quote priced.
//
// DiscountTotal is what promotions and the applied coupon take off; the
// product-markdown saving is already in the line prices and is no row
// here: one would count it twice. FreeShipping says a promotion waived
// the delivery.
//
// Discounts says which offer earned what, so no coupon is credited with
// an automatic promotion's money; RefusedCoupons are the attached codes
// the store refuses now (an item left the cart, a better offer won), in
// the ACP rejection vocabulary.
type Pricing struct {
	Lines          []PricedLine
	ItemsSubtotal  int64
	DiscountTotal  int64
	Discounts      []PricedDiscount
	RefusedCoupons []DiscountRejection
	FreeShipping   bool
	DeliveryFee    int64
	HasDelivery    bool
	PaymentFee     int64
	HasPaymentFee  bool
	Total          int64
	// Currency is the ISO 4217 code of every amount.
	Currency string
	// DeliveryRefused and PayWayRefused are the checkout's choices the
	// store no longer takes for this cart (it does not ship that way, the
	// cart outgrew the carrier, the pay way is not offered for the
	// delivery or no longer exists), left out of the price. The checkout
	// must choose again.
	DeliveryRefused *Refusal
	PayWayRefused   *Refusal
	// InputRefused is the store's refusal of the checkout's other data
	// (an email it does not take, a country or kind it does not know):
	// only the cart is priced, and the checkout must correct it.
	InputRefused *Refusal
	// ShippingOptions is every delivery the store offers to the
	// checkout's country, at the price this cart would be charged for
	// it; nil without a country, and once an order exists.
	ShippingOptions []django.QuoteShippingOption
	// PayWay is the pay-way the buyer selected, or nil when none is
	// selected yet, the store refused it, or the lookup failed. Callers
	// deciding whether an agent can settle a checkout unaided MUST read
	// this rather than the store's advertised set: the selection is the
	// authoritative fact for THIS checkout.
	PayWay *django.PayWay
	// Order is the placed order the totals were read from, nil before
	// one exists: a placed checkout's status and expiry are its order's
	// (Session.FollowOrder, Session.AwaitDecision).
	Order *django.Order
}

// Refusal is a choice the store refused, by Django's code and its own
// sentence in the request's language.
type Refusal struct {
	Code   string
	Detail string
}

// Refused is the first of the checkout's choices or data the store
// refuses for this cart, nil when it refuses none: an order cannot be
// placed until it is corrected.
func (p *Pricing) Refused() *Refusal {
	for _, r := range []*Refusal{
		p.DeliveryRefused, p.PayWayRefused, p.InputRefused,
	} {
		if r != nil {
			return r
		}
	}
	return nil
}

// ComputePricing fetches the cart fresh for its lines and Django's quote
// for every amount. Money totals are never computed by the gateway: what
// the checkout shows is what Django charges.
//
// A choice the store refuses (Pricing.DeliveryRefused, PayWayRefused) is
// left out and the rest is priced. A quote that cannot be read is an
// error: a checkout never shows a total that leaves out a row it could
// not price.
//
// Once an order exists it is what the buyer bought, and Django deletes the
// cart with it (on payment, for an online method), so the totals come from
// the order and the returned cart is nil.
func ComputePricing(
	ctx context.Context, dj *django.Client, t *tenant.Tenant, s *Session,
) (*Pricing, *django.Cart, error) {
	if s.OrderUUID != "" {
		p, err := orderPricing(ctx, dj, t, s.OrderUUID)
		return p, nil, err
	}
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
		p.Lines = append(p.Lines, PricedLine{
			LineID:     it.ID,
			ProductID:  it.Product.ID,
			Title:      tr.Name,
			ImagePath:  it.Product.MainImagePath,
			Quantity:   it.Quantity,
			UnitMinor:  unit,
			TotalMinor: lineTotal,
		})
	}

	q, err := p.quote(ctx, dj, t, s)
	if err != nil {
		return nil, nil, err
	}
	// Every amount is labelled with the store's currency: a quote in
	// another is a store whose settings disagree, and is not shown.
	if q.Currency != t.DefaultCurrency || (q.ExpectedTotal != nil &&
		q.ExpectedTotal.Currency != t.DefaultCurrency) {
		return nil, nil, fmt.Errorf("checkout: the store quotes %s, "+
			"its checkout shows %s", q.Currency, t.DefaultCurrency)
	}
	if err := p.addQuote(q, cart); err != nil {
		return nil, nil, err
	}
	if s.PayWayID > 0 && p.PayWayRefused == nil {
		// Whether the agent can settle the checkout is the pay way's
		// settlement, which the quote does not carry.
		if pw, err := dj.PayWayByID(
			ctx, t.Domain, t.DefaultLocale, s.PayWayID,
		); err == nil {
			p.PayWay = pw
		}
	}
	return p, cart, nil
}

// QuoteRequest is the quote of what the session has chosen: the very
// choices its order is placed with (Flow.place), the email included,
// since a promotion may depend on the buyer.
func QuoteRequest(s *Session) django.QuoteRequest {
	f := s.Fulfillment
	r := django.QuoteRequest{
		PayWayID:  s.PayWayID,
		CountryID: f.CountryCode,
		Email:     s.Buyer.Email,
	}
	if f.Kind != "" && f.CountryCode != "" {
		r.ShippingKind = f.Kind
		r.ShippingProviderCode = f.ProviderCode
		r.RegionID = f.RegionID
	}
	return r
}

// quote asks Django what the session's choices cost. A choice the store
// refuses is recorded on p and dropped, and the rest quoted again: the
// pay way is judged first, against the delivery, so at most both are.
// Any other refusal of the session's data leaves only the cart to price.
// An empty cart has nothing to price.
func (p *Pricing) quote(
	ctx context.Context, dj *django.Client, t *tenant.Tenant, s *Session,
) (*django.Quote, error) {
	req := QuoteRequest(s)
	for {
		q, err := dj.Quote(ctx, t.Domain, t.DefaultLocale, s.CartID, req)
		apiErr, ok := errors.AsType[*django.APIError](err)
		if err == nil || !ok {
			return q, err
		}
		refusal := &Refusal{Code: apiErr.Code, Detail: apiErr.Detail}
		switch {
		case payWayRefusal(apiErr.Code) && req.PayWayID > 0:
			p.PayWayRefused = refusal
			req.PayWayID = 0
		case shippingRefusal(apiErr.Code) && req.ShippingKind != "":
			p.DeliveryRefused = refusal
			req.ShippingKind, req.ShippingProviderCode = "", ""
			req.RegionID = ""
		case apiErr.Code == django.CodeCartEmpty:
			return &django.Quote{ItemsSubtotal: "0", DiscountTotal: "0",
				Currency: t.DefaultCurrency}, nil
		// The country is kept, so the deliveries stay listed, unless it
		// is what the store refuses.
		case errors.Is(err, django.ErrValidation) &&
			req != (django.QuoteRequest{CountryID: req.CountryID}):
			p.InputRefused = refusal
			req = django.QuoteRequest{CountryID: req.CountryID}
		case errors.Is(err, django.ErrValidation) && req.CountryID != "":
			p.InputRefused = refusal
			req = django.QuoteRequest{}
		default:
			return nil, err
		}
	}
}

// payWayRefusal reports whether code is one of Django's refusals of a
// pay way for a checkout: not offered for its delivery, or gone.
func payWayRefusal(code string) bool {
	return code == django.CodePayWayUnavailable ||
		code == django.CodePayWayNotFound
}

// shippingRefusal reports whether code is one of Django's refusals of a
// delivery for a cart.
func shippingRefusal(code string) bool {
	switch code {
	case django.CodeShippingUnavailable, django.CodeShippingParcelTooLarge,
		django.CodeShippingWeightExceeded:
		return true
	}
	return false
}

// addQuote copies the quote's amounts. The total is the quote's expected
// total when it has one (both a delivery and a pay way chosen); without,
// the rows it has, which no order is placed at.
func (p *Pricing) addQuote(q *django.Quote, cart *django.Cart) error {
	var err error
	if p.ItemsSubtotal, err = money.MinorUnits(q.ItemsSubtotal.String()); err != nil {
		return err
	}
	if p.DiscountTotal, err = money.MinorUnits(q.DiscountTotal.String()); err != nil {
		return err
	}
	if err := p.addDiscounts(q, cart); err != nil {
		return err
	}
	p.Currency = q.Currency
	p.ShippingOptions = q.ShippingOptions
	if q.Shipping != nil {
		if p.DeliveryFee, err = money.MinorUnits(q.Shipping.Price.String()); err != nil {
			return err
		}
		p.HasDelivery = true
		p.FreeShipping = q.Shipping.WaivedBy == django.FreeShippingByPromotion
	}
	if q.PaymentMethodFee != nil && !q.PaymentMethodFee.Waived {
		if p.PaymentFee, err = money.MinorUnits(
			q.PaymentMethodFee.Amount.String()); err != nil {
			return err
		}
		p.HasPaymentFee = p.PaymentFee > 0
	}
	if q.ExpectedTotal != nil {
		p.Total = q.ExpectedTotal.Amount
		return nil
	}
	p.Total = max(p.ItemsSubtotal-p.DiscountTotal+p.DeliveryFee+p.PaymentFee, 0)
	return nil
}

// orderPricing reads the totals of a placed order. Its discounts arrive as
// one sum, not per offer. A gift card is not one of them: it is a way to pay,
// so Django's grand total leaves it out, and counting it here would leave
// the rows short of that total.
func orderPricing(
	ctx context.Context, dj *django.Client, t *tenant.Tenant, orderUUID string,
) (*Pricing, error) {
	o, err := dj.PlacedOrder(ctx, t.Domain, t.DefaultLocale, orderUUID)
	if err != nil {
		return nil, err
	}
	p := &Pricing{
		Lines: make([]PricedLine, 0, len(o.Items)), HasDelivery: true,
		Order: o,
	}
	for _, it := range o.Items {
		unit, err := money.MinorUnits(it.Price.String())
		if err != nil {
			return nil, err
		}
		lineTotal, err := money.MinorUnits(it.TotalPrice.String())
		if err != nil {
			return nil, err
		}
		p.Lines = append(p.Lines, PricedLine{
			LineID:     it.Product.ID,
			ProductID:  it.Product.ID,
			Title:      it.ProductName,
			ImagePath:  it.Product.MainImagePath,
			Quantity:   it.Quantity,
			UnitMinor:  unit,
			TotalMinor: lineTotal,
		})
	}
	b := o.PricingBreakdown
	for _, field := range []struct {
		into *int64
		from string
	}{
		{&p.ItemsSubtotal, b.ItemsSubtotal.String()},
		{&p.DeliveryFee, b.ShippingCost.String()},
		{&p.PaymentFee, b.PaymentMethodFee.String()},
		{&p.Total, b.GrandTotal.String()},
	} {
		if *field.into, err = money.MinorUnits(field.from); err != nil {
			return nil, err
		}
	}
	for _, reduction := range []string{
		b.Discount.String(), b.LoyaltyDiscount.String(),
	} {
		amount, err := money.MinorUnits(reduction)
		if err != nil {
			return nil, err
		}
		p.DiscountTotal += amount
	}
	p.HasPaymentFee = p.PaymentFee > 0
	p.Currency = b.Currency
	return p, nil
}

// addDiscounts copies the quote's per-offer breakdown and its verdict on
// the attached coupon. A coupon that applies with no money of its own
// (free shipping, a gift) takes nothing off, so the quote does not list
// it: the cart names it, and it is listed at 0. The coupon the quote
// says order creation refuses is a rejection, and so is one the cart
// says is refused without blocking the order.
func (p *Pricing) addDiscounts(q *django.Quote, cart *django.Cart) error {
	monetary := map[string]bool{}
	for _, ap := range q.AppliedPromotions {
		amount, err := money.MinorUnits(ap.Amount.String())
		if err != nil {
			return err
		}
		p.Discounts = append(p.Discounts, PricedDiscount{
			PromotionID: ap.PromotionID,
			Name:        ap.Name,
			Code:        ap.Code,
			Amount:      amount,
		})
		if ap.Code != "" {
			monetary[ap.Code] = true
		}
	}
	if b := q.BlockingCoupon; b != nil {
		p.RefusedCoupons = append(p.RefusedCoupons, DiscountRejection{
			Code:   b.Code,
			Reason: mapDiscountReason(b.Reason),
		})
	}
	for _, c := range cart.AppliedCoupons {
		switch {
		case q.BlockingCoupon != nil && q.BlockingCoupon.Code == c.Code:
		// Refused without blocking the order (another offer won, it
		// applies to nothing in the cart): the cart says why.
		case c.Rejection != "":
			p.RefusedCoupons = append(p.RefusedCoupons, DiscountRejection{
				Code:   c.Code,
				Reason: mapDiscountReason(c.Rejection),
			})
		case !monetary[c.Code]:
			p.Discounts = append(p.Discounts, PricedDiscount{
				Name: c.Code,
				Code: c.Code,
			})
		}
	}
	return nil
}
