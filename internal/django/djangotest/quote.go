// Package djangotest answers, for the gateway's tests, the Django calls
// whose answer Django computes from a store's data rather than records:
// the checkout quote. It prices as Django's OrderQuoteService does for
// the cases the gateway reads, from the same recorded fixtures the other
// fake routes serve, so a test's totals follow its cart, its options
// and its pay ways.
package djangotest

import (
	"encoding/json"
	"net/http"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
)

// Store is what a quote is computed from: the bodies Django serves for
// the cart (GET /cart), the delivery options (GET /shipping/options) and
// each pay way by id (GET /pay_way/{id}).
type Store struct {
	Cart            []byte
	ShippingOptions []byte
	PayWays         map[int64][]byte
}

type cartBody struct {
	Items                 []json.RawMessage `json:"items"`
	TotalPrice            json.Number       `json:"totalPrice"`
	PromotionDiscount     json.Number       `json:"promotionDiscount"`
	FreeThresholdValue    json.Number       `json:"freeThresholdValue"`
	PromotionFreeShipping bool              `json:"promotionFreeShipping"`
	AppliedPromotions     []json.RawMessage `json:"appliedPromotions"`
	Currency              string            `json:"currency"`
}

type optionBody struct {
	ProviderCode      string      `json:"providerCode"`
	ProviderName      string      `json:"providerName"`
	Kind              string      `json:"kind"`
	Price             json.Number `json:"price"`
	ExceedsMaxWeight  bool        `json:"exceedsMaxWeight"`
	ExceedsParcelSize bool        `json:"exceedsParcelSize"`
}

type payWayBody struct {
	Cost          json.Number `json:"cost"`
	FreeThreshold json.Number `json:"freeThreshold"`
}

// Quote answers POST /cart/quote as Django does: the items after the
// cart's promotions, the chosen delivery (free when a promotion waives
// it; refused with its code when the store does not ship that way or the
// cart does not fit), the chosen pay way's fee (waived from its free
// threshold; refused while it is not one of Store.PayWays), the options
// for the country, and the expected total once both are chosen.
func (s Store) Quote(w http.ResponseWriter, r *http.Request) {
	var req django.QuoteRequest
	var cart cartBody
	var options []optionBody
	if json.NewDecoder(r.Body).Decode(&req) != nil ||
		json.Unmarshal(s.Cart, &cart) != nil ||
		json.Unmarshal(s.ShippingOptions, &options) != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if len(cart.Items) == 0 {
		refuse(w, django.CodeCartEmpty)
		return
	}
	items := minor(cart.TotalPrice)
	discount := minor(cart.PromotionDiscount)
	value := minor(cart.FreeThresholdValue)
	price := func(o optionBody) (int64, any) {
		if cart.PromotionFreeShipping {
			return 0, django.FreeShippingByPromotion
		}
		return minor(o.Price), nil
	}

	var fee *int64
	if req.PayWayID > 0 {
		raw, ok := s.PayWays[req.PayWayID]
		var pw payWayBody
		if !ok || json.Unmarshal(raw, &pw) != nil {
			refuse(w, django.CodePayWayUnavailable)
			return
		}
		f := minor(pw.Cost)
		if threshold := minor(pw.FreeThreshold); threshold > 0 &&
			value >= threshold {
			f = 0
		}
		fee = &f
	}

	var shipping map[string]any
	var delivery *int64
	if req.ShippingKind != "" && req.CountryID != "" {
		chosen := -1
		for i, o := range options {
			if o.Kind == req.ShippingKind && (req.ShippingProviderCode == "" ||
				o.ProviderCode == req.ShippingProviderCode) {
				chosen = i
				break
			}
		}
		switch {
		case chosen < 0:
			refuse(w, django.CodeShippingUnavailable)
			return
		case options[chosen].ExceedsMaxWeight:
			refuse(w, django.CodeShippingWeightExceeded)
			return
		case options[chosen].ExceedsParcelSize:
			refuse(w, django.CodeShippingParcelTooLarge)
			return
		}
		d, waivedBy := price(options[chosen])
		delivery = &d
		shipping = map[string]any{"price": amount(d), "waivedBy": waivedBy}
	}

	var listed []map[string]any
	if req.CountryID != "" {
		listed = []map[string]any{}
		for _, o := range options {
			p, waivedBy := price(o)
			listed = append(listed, map[string]any{
				"providerCode": o.ProviderCode, "providerName": o.ProviderName,
				"kind": o.Kind, "price": amount(p), "waivedBy": waivedBy,
				"exceedsMaxWeight":  o.ExceedsMaxWeight,
				"exceedsParcelSize": o.ExceedsParcelSize,
			})
		}
	}

	body := map[string]any{
		"itemsSubtotal": amount(items), "discountTotal": amount(discount),
		"appliedPromotions": cart.AppliedPromotions, "blockingCoupon": nil,
		"freeThresholdValue": amount(value), "shipping": shipping,
		"paymentMethodFee": nil, "loyaltyDiscount": amount(0),
		"giftCardAmount": nil, "grandTotal": nil, "amountDue": nil,
		"currency": cart.Currency, "expectedTotal": nil,
		"shippingOptions": listed, "payWays": []any{},
	}
	if body["appliedPromotions"] == nil {
		body["appliedPromotions"] = []any{}
	}
	if fee != nil {
		body["paymentMethodFee"] = map[string]any{
			"amount": amount(*fee), "waived": *fee == 0}
	}
	if fee != nil && delivery != nil {
		total := max(items-discount+*delivery+*fee, 0)
		body["grandTotal"], body["amountDue"] = amount(total), amount(total)
		body["giftCardAmount"] = amount(0)
		body["expectedTotal"] = map[string]any{
			"amount": total, "currency": cart.Currency}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// refuse answers a choice the store refuses, as the quote does.
func refuse(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"detail": "Refused: " + code, "reason": code, "code": code})
}

// minor is a recorded amount in minor units; an absent one is 0.
func minor(n json.Number) int64 {
	if n == "" {
		return 0
	}
	v, err := money.MinorUnits(n.String())
	if err != nil {
		return 0
	}
	return v
}

// amount is minor units as the JSON number Django sends.
func amount(minor int64) json.Number {
	return json.Number(money.Format(minor))
}
