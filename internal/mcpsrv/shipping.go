package mcpsrv

import (
	"context"
	"errors"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

type ShippingOptionsIn struct {
	CountryCode string `json:"countryCode" jsonschema:"ISO 3166-1 alpha-2 destination, e.g. GR"`
	OrderValue  string `json:"orderValue,omitempty" jsonschema:"cart total as decimal string, used for free-shipping checks"`
	WeightGrams int    `json:"weightGrams,omitempty"`
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
	opts, err := h.deps.Django.ShippingOptions(ctx, t.Domain, t.DefaultLocale, q)
	if err != nil {
		return nil, out, upstreamErr(err, "shipping options are unavailable")
	}
	for _, o := range opts {
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
		ctx, t.Domain, t.DefaultLocale, in.CountryCode); err == nil {
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
	PostalCode string `json:"postalCode" jsonschema:"Greek postal code, e.g. 10434"`
	City       string `json:"city,omitempty" jsonschema:"city name, improves BOX NOW matching"`
	Street     string `json:"street,omitempty" jsonschema:"street and number, improves BOX NOW matching"`
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
	Points []PickupPoint `json:"points"`
}

const maxAcsPoints = 10

func (h *handlers) findPickupPoints(
	ctx context.Context, _ *mcp.CallToolRequest, in PickupPointsIn,
) (*mcp.CallToolResult, PickupPointsOut, error) {
	var out PickupPointsOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	if in.PostalCode == "" {
		return nil, out, errors.New("postalCode is required")
	}

	stations, acsErr := h.deps.Django.AcsNearestStations(
		ctx, t.Domain, t.DefaultLocale, in.PostalCode, in.City)
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

	// BOX NOW's nearest lookup proxies their live API and needs an
	// address; skip quietly when the extra fields are absent or the
	// upstream declines — ACS results remain useful alone.
	boxnowIncluded := false
	if in.City != "" {
		if l, err := h.deps.Django.BoxNowNearestLocker(
			ctx, t.Domain, t.DefaultLocale, django.BoxNowNearestRequest{
				City:       in.City,
				Street:     in.Street,
				PostalCode: in.PostalCode,
			}); err == nil {
			boxnowIncluded = true
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
		if acsErr != nil {
			return nil, out, upstreamErr(acsErr,
				"pickup point lookup is unavailable")
		}
		return nil, out, errors.New(
			"no pickup points found near that postal code; try a " +
				"neighbouring postal code or add the city name")
	}

	note := ""
	if !boxnowIncluded {
		note = " Provide city (and street) to also check BOX NOW lockers."
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
	ID                   int64  `json:"id" jsonschema:"payWayId used at checkout"`
	Label                string `json:"label"`
	Description          string `json:"description,omitempty"`
	Cost                 string `json:"cost"`
	FreeThreshold        string `json:"freeThreshold"`
	Currency             string `json:"currency"`
	IsOnlinePayment      bool   `json:"isOnlinePayment"`
	RequiresConfirmation bool   `json:"requiresConfirmation"`
	ProviderCode         string `json:"providerCode"`
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

	page, err := h.deps.Django.PayWays(ctx, t.Domain, t.DefaultLocale,
		in.ShippingProviderCode, in.ShippingKind)
	if err != nil {
		return nil, out, upstreamErr(err, "payment methods are unavailable")
	}
	for _, pw := range page.Results {
		if !pw.Active {
			continue
		}
		tr := django.Localized(pw.Translations, t.DefaultLocale)
		out.Methods = append(out.Methods, PaymentMethodOut{
			ID:                   pw.ID,
			Label:                tr.Name,
			Description:          tr.Description,
			Cost:                 num(pw.Cost),
			FreeThreshold:        num(pw.FreeThreshold),
			Currency:             t.DefaultCurrency,
			IsOnlinePayment:      pw.IsOnlinePayment,
			RequiresConfirmation: pw.RequiresConfirmation,
			ProviderCode:         pw.ProviderCode,
		})
	}
	return textResult(
		"%d payment methods available. Method costs are waived above "+
			"each freeThreshold.",
		len(out.Methods),
	), out, nil
}
