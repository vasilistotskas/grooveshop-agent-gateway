package mcpsrv

import (
	"cmp"
	"context"
	"errors"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

type ShippingOptionsIn struct {
	CountryCode string `json:"countryCode" jsonschema:"ISO 3166-1 alpha-2 destination, e.g. GR"`
	OrderValue  string `json:"orderValue,omitempty" jsonschema:"the cart's freeThresholdValue from get_cart as a decimal string: the items after discounts, excluding shipping, which free-shipping thresholds are judged on; ignored when the call names a cart that holds items, whose own value is used"`
	WeightGrams int    `json:"weightGrams,omitempty" jsonschema:"for a destination with no cart: the weight to judge each option's weight cap on, without it none is left out for weight; ignored when the call names a cart that holds items, whose own weight Django judges"`
	CartID      string `json:"cartId,omitempty" jsonschema:"the cartId a cart tool returned: its options are priced as its order would be charged, and one too heavy or too large for it is left out"`
}

type ShippingOptionOut struct {
	ProviderCode string `json:"providerCode"`
	ProviderName string `json:"providerName"`
	Kind         string `json:"kind"`
	Price        string `json:"price"`
	Currency     string `json:"currency"`
}

type ShippingOptionsOut struct {
	Options      []ShippingOptionOut `json:"options"`
	FreeShipping []FreeShippingOut   `json:"freeShipping,omitempty"`
}

// FreeShippingOut is the order value from which a carrier's delivery of
// a kind is free.
type FreeShippingOut struct {
	ProviderCode string `json:"providerCode"`
	Kind         string `json:"kind"`
	Threshold    string `json:"threshold"`
}

func (h *handlers) getShippingOptions(
	ctx context.Context, _ *mcp.CallToolRequest, in ShippingOptionsIn,
) (*mcp.CallToolResult, ShippingOptionsOut, error) {
	var out ShippingOptionsOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	lang := django.Language(ctx, t.TenantConfig)
	if in.CountryCode == "" {
		return nil, out, errors.New("countryCode is required (e.g. GR)")
	}

	// The cart is the chat shopper's (their cart token) or cartId: its
	// options are priced by the store's checkout quote, as its order
	// would be charged, whatever value or weight the model passed. With
	// neither, the value and weight given are priced, and Django flags
	// no option for size.
	options, err := h.shippingOptions(ctx, t, lang, in)
	if err != nil {
		return nil, out, err
	}
	out.Options = make([]ShippingOptionOut, 0, len(options))
	for _, o := range options {
		// Listed upstream so a storefront can explain the greyed-out
		// option; an agent can only act on options it may choose.
		if o.fits {
			out.Options = append(out.Options, o.ShippingOptionOut)
		}
	}

	// Free-shipping thresholds are informative, not load-bearing — a
	// failure here must not hide the shipping options themselves.
	if info, err := h.deps.Django.FreeShippingInfo(
		ctx, t.Domain, lang, in.CountryCode); err == nil {
		for _, p := range info.Providers {
			out.FreeShipping = append(out.FreeShipping, FreeShippingOut{
				ProviderCode: p.ProviderCode,
				Kind:         p.Kind,
				Threshold:    num(p.Threshold),
			})
		}
	}

	return textResult(
		"%d delivery options to %s. pickup_point deliveries need a "+
			"locker/shop chosen via find_pickup_points.",
		len(out.Options), in.CountryCode,
	), out, nil
}

// pricedOption is one delivery option and whether the cart fits it.
type pricedOption struct {
	ShippingOptionOut
	fits bool
}

// shippingOptions prices the deliveries to in.CountryCode: by the
// checkout quote of the named cart (or the chat shopper's), else, and for
// a cart with nothing in it yet, by the value and weight the call gives.
func (h *handlers) shippingOptions(
	ctx context.Context, t *tenant.Tenant, lang string, in ShippingOptionsIn,
) ([]pricedOption, error) {
	if in.CartID != "" || django.HasCartToken(ctx) {
		q, err := h.deps.Django.Quote(ctx, t.Domain, lang, in.CartID,
			django.QuoteRequest{CountryID: in.CountryCode})
		apiErr, refused := errors.AsType[*django.APIError](err)
		switch {
		// An empty cart has nothing to judge: the options are the
		// store's own, asked for no cart (not even the shopper's token).
		case refused && apiErr.Code == django.CodeCartEmpty:
			in.CartID = ""
			return h.bareShippingOptions(django.WithCartToken(ctx, nil), t,
				lang, in)
		case err != nil:
			return nil, h.cartGoneErr(err)
		}
		out := make([]pricedOption, 0, len(q.ShippingOptions))
		for _, o := range q.ShippingOptions {
			out = append(out, pricedOption{fits: o.FitsCart(),
				ProviderCode: o.ProviderCode, ProviderName: o.ProviderName,
				Kind: o.Kind, Price: num(o.Price), Currency: q.Currency})
		}
		return out, nil
	}
	return h.bareShippingOptions(ctx, t, lang, in)
}

// bareShippingOptions prices the deliveries on the value and weight the
// call gives, with no cart judged.
func (h *handlers) bareShippingOptions(
	ctx context.Context, t *tenant.Tenant, lang string, in ShippingOptionsIn,
) ([]pricedOption, error) {
	q := django.ShippingQuery{
		CountryCode:      in.CountryCode,
		OrderValueAmount: in.OrderValue,
		Currency:         t.DefaultCurrency,
	}
	if in.WeightGrams > 0 {
		q.WeightGrams = strconv.Itoa(in.WeightGrams)
	}
	opts, err := h.deps.Django.ShippingOptions(ctx, t.Domain, lang, "", q)
	if err != nil {
		return nil, upstreamErr(err, "shipping options are unavailable")
	}
	out := make([]pricedOption, 0, len(opts))
	for _, o := range opts {
		out = append(out, pricedOption{fits: o.FitsCart(),
			ProviderCode: o.ProviderCode, ProviderName: o.ProviderName,
			Kind: o.Kind, Price: num(o.Price), Currency: o.Currency})
	}
	return out, nil
}

type PickupPointsIn struct {
	CountryCode string `json:"countryCode" jsonschema:"ISO 3166-1 alpha-2 country of the postal code, e.g. GR"`
	PostalCode  string `json:"postalCode" jsonschema:"postal code to search near, e.g. 10434"`
	City        string `json:"city,omitempty" jsonschema:"the shopper's city; BOX NOW lockers are searched only with city and street"`
	Street      string `json:"street,omitempty" jsonschema:"the shopper's street and number; BOX NOW lockers are searched only with city and street"`
}

type PickupPoint struct {
	Provider     string `json:"provider" jsonschema:"acs or boxnow"`
	ID           string `json:"id" jsonschema:"provider-specific point id used at checkout"`
	BranchCode   string `json:"branchCode,omitempty" jsonschema:"ACS branch code, required with the ACS id at checkout"`
	Name         string `json:"name"`
	Address      string `json:"address"`
	City         string `json:"city"`
	PostalCode   string `json:"postalCode"`
	Lat          string `json:"lat,omitempty"`
	Lng          string `json:"lng,omitempty"`
	MaxWeightKg  string `json:"maxWeightKg,omitempty"`
	WorkingHours string `json:"workingHours,omitempty"`
	Note         string `json:"note,omitempty"`
}

type PickupPointsOut struct {
	// Carriers are the pickup carriers the store offers to the country
	// for the cart, whose networks were searched or need more input.
	Carriers []string      `json:"carriers"`
	Points   []PickupPoint `json:"points"`
}

const maxAcsPoints = 10

// pickupCarriers are the providers the store delivers to pickup points
// with in countryCode that the cart fits, among those whose networks the
// gateway can search. A chat shopper's cart is judged through their cart
// token, as get_shipping_options judges it.
func (h *handlers) pickupCarriers(
	ctx context.Context, t *tenant.Tenant, lang, countryCode string,
) (offered map[string]bool, pickup bool, err error) {
	opts, err := h.deps.Django.ShippingOptions(ctx, t.Domain, lang, "",
		django.ShippingQuery{CountryCode: countryCode,
			Currency: t.DefaultCurrency})
	if err != nil {
		return nil, false, err
	}
	offered = map[string]bool{}
	for _, o := range opts {
		if o.Kind != django.ShippingKindPickupPoint {
			continue
		}
		pickup = true
		switch o.ProviderCode {
		case django.ShippingProviderACS, django.ShippingProviderBoxNow:
			if o.FitsCart() {
				offered[o.ProviderCode] = true
			}
		}
	}
	return offered, pickup, nil
}

func (h *handlers) findPickupPoints(
	ctx context.Context, _ *mcp.CallToolRequest, in PickupPointsIn,
) (*mcp.CallToolResult, PickupPointsOut, error) {
	out := PickupPointsOut{Carriers: []string{}, Points: []PickupPoint{}}
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	lang := django.Language(ctx, t.TenantConfig)
	if in.CountryCode == "" {
		return nil, out, errors.New("countryCode is required (e.g. GR)")
	}
	if in.PostalCode == "" {
		return nil, out, errors.New("postalCode is required")
	}

	carriers, anyPickup, err := h.pickupCarriers(
		ctx, t, lang, in.CountryCode)
	if err != nil {
		return nil, out, upstreamErr(err, "pickup point lookup is unavailable")
	}
	// Answers, not errors: no other postal code would change them.
	switch {
	case !anyPickup:
		return textResult(
			"This store does not deliver to pickup points in %s; offer "+
				"its home delivery (get_shipping_options) instead.",
			in.CountryCode), out, nil
	case len(carriers) == 0:
		return textResult(
			"The cart is too large or too heavy for this store's pickup "+
				"points in %s; offer its home delivery "+
				"(get_shipping_options) instead.",
			in.CountryCode), out, nil
	}
	for _, c := range []string{
		django.ShippingProviderACS, django.ShippingProviderBoxNow,
	} {
		if carriers[c] {
			out.Carriers = append(out.Carriers, c)
		}
	}

	var acsErr error
	if carriers[django.ShippingProviderACS] {
		var stations []django.AcsStation
		stations, acsErr = h.deps.Django.AcsNearestStations(
			ctx, t.Domain, lang, in.PostalCode, in.City)
		for i, s := range stations {
			if i >= maxAcsPoints {
				break
			}
			out.Points = append(out.Points, PickupPoint{
				Provider:     django.ShippingProviderACS,
				ID:           s.ExternalID,
				BranchCode:   s.BranchCode,
				Name:         s.Name,
				Address:      s.AddressLine1,
				City:         s.City,
				PostalCode:   s.PostalCode,
				Lat:          s.Lat,
				Lng:          s.Lng,
				MaxWeightKg:  s.MaxWeightKg,
				WorkingHours: s.WorkingHours,
			})
		}
	}

	// BOX NOW's nearest lookup proxies their live API, which Django calls
	// only with a full address: city, street and postal code are each
	// required there, so a call without one is a certain 400. Django
	// answers every BOX NOW refusal with a 400, the ordinary "no locker
	// near that address" among them, so a 400 is no point found; a BOX
	// NOW outage reads the same until Django answers it as a 5xx. Any
	// other failure is kept: with no point found, it is the answer.
	note := ""
	var boxNowErr error
	if carriers[django.ShippingProviderBoxNow] {
		if in.City == "" || in.Street == "" {
			note = "BOX NOW lockers are searched only with the " +
				"shopper's city and street (with number): ask the " +
				"shopper for them, then call again."
		} else {
			l, err := h.deps.Django.BoxNowNearestLocker(
				ctx, t.Domain, lang, django.BoxNowNearestRequest{
					City:       in.City,
					Street:     in.Street,
					PostalCode: in.PostalCode,
				})
			switch {
			case err == nil:
				out.Points = append(out.Points, PickupPoint{
					Provider:   django.ShippingProviderBoxNow,
					ID:         l.ID,
					Name:       l.Name,
					Address:    l.AddressLine1,
					City:       l.AddressLine2,
					PostalCode: l.PostalCode,
					Lat:        l.Lat,
					Lng:        l.Lng,
					Note:       l.Note,
				})
			case !errors.Is(err, django.ErrValidation):
				boxNowErr = err
			}
		}
	}

	if len(out.Points) == 0 {
		if err := cmp.Or(acsErr, boxNowErr); err != nil {
			err = upstreamErr(err, "pickup point lookup is unavailable")
			if note != "" {
				err = withHint(err, note)
			}
			return nil, out, err
		}
		if note != "" {
			return textResult("No pickup points listed yet. %s", note),
				out, nil
		}
		return nil, out, errors.New(
			"no pickup points found near that postal code; try a " +
				"neighbouring postal code or add the city name")
	}
	if note != "" {
		note = " " + note
	}
	return textResult(
		"Found %d pickup points near %s.%s",
		len(out.Points), in.PostalCode, note,
	), out, nil
}

type PaymentMethodsIn struct {
	ShippingProviderCode string `json:"shippingProviderCode,omitempty" jsonschema:"filter to methods compatible with this carrier, e.g. acs, boxnow"`
	ShippingKind         string `json:"shippingKind,omitempty" jsonschema:"home_delivery or pickup_point"`
}

type PaymentMethodOut struct {
	ID            int64  `json:"id" jsonschema:"payWayId used at checkout"`
	Label         string `json:"label"`
	Description   string `json:"description,omitempty"`
	Cost          string `json:"cost"`
	FreeThreshold string `json:"freeThreshold"`
	Currency      string `json:"currency"`
	ProviderCode  string `json:"providerCode"`
	// Replaces the isOnlinePayment / requiresConfirmation pair, which
	// were deprecated mirrors upstream and could not express the
	// difference between cash to a courier and a card at a locker
	// terminal — two methods that ride different carriers and cannot be
	// swapped. Agents branch on this.
	Settlement string `json:"settlement" jsonschema:"how the money changes hands: online (paid at checkout), courier_cash (cash or card to the courier on delivery), carrier_terminal (card at the carrier's locker terminal on pickup), offline_transfer (settled off-platform, e.g. bank transfer)"`
}

type PaymentMethodsOut struct {
	Methods []PaymentMethodOut `json:"methods"`
}

func (h *handlers) getPaymentMethods(
	ctx context.Context, _ *mcp.CallToolRequest, in PaymentMethodsIn,
) (*mcp.CallToolResult, PaymentMethodsOut, error) {
	var out PaymentMethodsOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	lang := django.Language(ctx, t.TenantConfig)

	page, err := h.deps.Django.PayWays(ctx, t.Domain, lang,
		in.ShippingProviderCode, in.ShippingKind)
	if err != nil {
		return nil, out, upstreamErr(err, "payment methods are unavailable")
	}
	out.Methods = make([]PaymentMethodOut, 0, len(page.Results))
	for _, pw := range page.Results {
		if !pw.Active {
			continue
		}
		label, err := pw.Label(lang)
		if err != nil {
			return nil, out, err
		}
		tr := django.Localized(pw.Translations, lang)
		out.Methods = append(out.Methods, PaymentMethodOut{
			ID:            pw.ID,
			Label:         label,
			Description:   tr.Description,
			Cost:          num(pw.Cost),
			FreeThreshold: num(pw.FreeThreshold),
			Currency:      t.DefaultCurrency,
			ProviderCode:  pw.ProviderCode,
			Settlement:    pw.Settlement,
		})
	}
	return textResult(
		"%d payment methods available. A method's cost is waived once "+
			"the cart's freeThresholdValue (get_cart) reaches its "+
			"freeThreshold.",
		len(out.Methods),
	), out, nil
}
