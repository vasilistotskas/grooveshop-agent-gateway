package mcpsrv

import (
	"context"
	"errors"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

type ShippingOptionsIn struct {
	CountryCode string `json:"countryCode" jsonschema:"ISO 3166-1 alpha-2 destination, e.g. GR"`
	OrderValue  string `json:"orderValue,omitempty" jsonschema:"cart total as decimal string, used for free-shipping checks"`
	WeightGrams int    `json:"weightGrams,omitempty" jsonschema:"the cart's totalWeightGrams from get_cart; without it no option is left out for its weight cap"`
	CartID      string `json:"cartId,omitempty" jsonschema:"cart id from create_cart or add_to_cart; without it no option is left out for being too large for the cart"`
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
	FreeShipping []struct {
		ProviderCode string `json:"providerCode"`
		Kind         string `json:"kind"`
		Threshold    string `json:"threshold"`
	} `json:"freeShipping,omitempty"`
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

	q := django.ShippingQuery{
		CountryCode:      in.CountryCode,
		OrderValueAmount: in.OrderValue,
		Currency:         t.DefaultCurrency,
	}
	if in.WeightGrams > 0 {
		q.WeightGrams = strconv.Itoa(in.WeightGrams)
	}
	// The cart is the chat shopper's (their cart token) or cartId; with
	// neither, Django flags no option for size.
	opts, err := h.deps.Django.ShippingOptions(
		ctx, t.Domain, lang, in.CartID, q)
	if err != nil {
		return nil, out, upstreamErr(err, "shipping options are unavailable")
	}
	for _, o := range opts {
		// Listed upstream so a storefront can explain the greyed-out
		// option; an agent can only act on options it may choose.
		if !o.FitsCart() {
			continue
		}
		out.Options = append(out.Options, ShippingOptionOut{
			ProviderCode: o.ProviderCode,
			ProviderName: o.ProviderName,
			Kind:         o.Kind,
			Price:        num(o.Price),
			Currency:     o.Currency,
		})
	}

	// Free-shipping thresholds are informative, not load-bearing — a
	// failure here must not hide the shipping options themselves.
	if info, err := h.deps.Django.FreeShippingInfo(
		ctx, t.Domain, lang, in.CountryCode); err == nil {
		for _, p := range info.Providers {
			out.FreeShipping = append(out.FreeShipping, struct {
				ProviderCode string `json:"providerCode"`
				Kind         string `json:"kind"`
				Threshold    string `json:"threshold"`
			}{p.ProviderCode, p.Kind, num(p.Threshold)})
		}
	}

	return textResult(
		"%d delivery options to %s. pickup_point deliveries need a "+
			"locker/shop chosen via find_pickup_points.",
		len(out.Options), in.CountryCode,
	), out, nil
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
	// required there, so a call without one is a certain 400. When BOX
	// NOW declines, the other network's results stand alone.
	note := ""
	if carriers[django.ShippingProviderBoxNow] {
		if in.City == "" || in.Street == "" {
			note = " BOX NOW lockers are searched only with the " +
				"shopper's city and street (with number): ask the " +
				"shopper for them, then call again."
		} else if l, err := h.deps.Django.BoxNowNearestLocker(
			ctx, t.Domain, lang, django.BoxNowNearestRequest{
				City:       in.City,
				Street:     in.Street,
				PostalCode: in.PostalCode,
			}); err == nil {
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
		}
	}

	if len(out.Points) == 0 {
		if note != "" {
			return textResult("No pickup points listed yet.%s", note),
				out, nil
		}
		if acsErr != nil {
			return nil, out, upstreamErr(acsErr,
				"pickup point lookup is unavailable")
		}
		return nil, out, errors.New(
			"no pickup points found near that postal code; try a " +
				"neighbouring postal code or add the city name")
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
		"%d payment methods available. Method costs are waived above "+
			"each freeThreshold.",
		len(out.Methods),
	), out, nil
}
