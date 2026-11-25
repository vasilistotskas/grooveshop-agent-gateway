package ucp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Fulfillment is the fulfillment extension's member
// (shopping/types/fulfillment.json), on a checkout whose platform
// negotiated it. The store fulfils a whole cart one way: a `shipping`
// method, home delivery to one address, and, where the store delivers to
// pickup points, a `pickup` method, its points the business's locations.
// The method that holds the cart's lines is the one chosen.
type Fulfillment struct {
	Methods []FulfillmentMethod `json:"methods"`
}

// FulfillmentMethod is a fulfillment method (fulfillment_method.json).
type FulfillmentMethod struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	LineItemIDs []string `json:"line_item_ids"`
	// Destinations is the shipping address the platform wrote, or the
	// pickup points the business lists, the selected one among them.
	Destinations          []Destination `json:"destinations,omitempty"`
	SelectedDestinationID string        `json:"selected_destination_id,omitempty"`
	// Groups is the one group of the cart's lines, once its options can
	// be priced: the delivery country is known (shipping), a point is
	// selected (pickup).
	Groups []FulfillmentGroup `json:"groups,omitempty"`
}

// Destination is a method's destination (fulfillment_destination.json):
// a shipping address (shipping_destination.json, the postal address
// flat) or a business location (location_destination.json, a name and
// its address).
type Destination struct {
	Type            string `json:"type"`
	ID              string `json:"id"`
	StreetAddress   string `json:"street_address,omitempty"`
	ExtendedAddress string `json:"extended_address,omitempty"`
	AddressLocality string `json:"address_locality,omitempty"`
	AddressRegion   string `json:"address_region,omitempty"`
	AddressCountry  string `json:"address_country,omitempty"`
	PostalCode      string `json:"postal_code,omitempty"`
	// Name and Address are a business location's.
	Name    string         `json:"name,omitempty"`
	Address *PostalAddress `json:"address,omitempty"`
}

// PostalAddress is a business location's address (postal_address.json).
type PostalAddress struct {
	StreetAddress   string `json:"street_address,omitempty"`
	AddressLocality string `json:"address_locality,omitempty"`
	AddressCountry  string `json:"address_country,omitempty"`
	PostalCode      string `json:"postal_code,omitempty"`
}

// FulfillmentGroup is the package of the cart's lines and its options
// (fulfillment_group.json). SelectedOptionID is null until one is
// selected.
type FulfillmentGroup struct {
	ID               string              `json:"id"`
	LineItemIDs      []string            `json:"line_item_ids"`
	Options          []FulfillmentOption `json:"options"`
	SelectedOptionID *string             `json:"selected_option_id"`
}

// FulfillmentOption is one carrier that delivers the cart
// (fulfillment_option.json): its id is the carrier's provider code, and
// its total what the cart's delivery costs with it.
type FulfillmentOption struct {
	ID      string          `json:"id"`
	Title   string          `json:"title"`
	Carrier string          `json:"carrier,omitempty"`
	Totals  []LineItemTotal `json:"totals"`
}

// The ids and types this business gives the parts of the ways it
// fulfils a cart: a platform sends them back on update.
const (
	FulfillmentMethodID   = "shipping"
	FulfillmentMethodType = "shipping"
	PickupMethodID        = "pickup"
	PickupMethodType      = "pickup"
	DestinationID         = "shipping_address"
	DestinationType       = "shipping_address"
	LocationType          = "business_location"
	FulfillmentGroupID    = "parcel"
	PickupGroupID         = "pickup_parcel"
)

// The paths, into a checkout, of what its fulfillment needs. The
// shipping method is the first, the pickup method the second.
const (
	pathDestinations = "$.fulfillment.methods[0].destinations"
	pathStreet       = "$.fulfillment.methods[0].destinations[0].street_address"
	pathRegion       = "$.fulfillment.methods[0].destinations[0].address_region"
	pathCountry      = "$.fulfillment.methods[0].destinations[0].address_country"
	pathShipping     = "$.fulfillment.methods[0]"
	pathOption       = "$.fulfillment.methods[0].groups[0].selected_option_id"
	pathPoint        = "$.fulfillment.methods[1].selected_destination_id"
)

// maxNearPoints is how many pickup points near the buyer a checkout lists.
const maxNearPoints = 5

// PickupLocation is a pickup point as the business names it to a
// platform: its carrier, its country, and the carrier's own ids (an ACS
// station is an id and a branch). Its DestinationID is opaque to a
// platform, which gets it from a checkout's pickup method or from
// find_pickup_points and sends it back as selected_destination_id.
type PickupLocation struct {
	Provider string
	Country  string
	ID       string
	Branch   string
}

// DestinationID is the location's id: its parts, each escaped, joined by
// colons.
func (l PickupLocation) DestinationID() string {
	parts := []string{l.Provider, l.Country, l.ID}
	if l.Provider == django.ShippingProviderACS {
		parts = append(parts, l.Branch)
	}
	for i, p := range parts {
		parts[i] = url.QueryEscape(p)
	}
	return strings.Join(parts, ":")
}

// ErrUnknownLocation: a destination id this business never gives.
var ErrUnknownLocation = errors.New("ucp: not a pickup location id")

// ParsePickupLocation reads a DestinationID back: ErrUnknownLocation for
// one this business could not have given.
func ParsePickupLocation(id string) (PickupLocation, error) {
	parts := strings.Split(id, ":")
	for i, p := range parts {
		var err error
		parts[i], err = url.QueryUnescape(p)
		// Every part is given but an ACS station's branch, which the
		// catalogue may hold blank (a station that is one row).
		blankBranch := i == 3 && parts[0] == django.ShippingProviderACS
		if err != nil || parts[i] == "" && !blankBranch {
			return PickupLocation{}, ErrUnknownLocation
		}
	}
	// The country is upper-case, as every id this business gives names it.
	switch {
	case len(parts) < 2 || !checkout.IsCountryCode(parts[1]) ||
		parts[1] != strings.ToUpper(parts[1]):
		return PickupLocation{}, ErrUnknownLocation
	case len(parts) == 3 && parts[0] == django.ShippingProviderBoxNow:
		return PickupLocation{Provider: parts[0], Country: parts[1],
			ID: parts[2]}, nil
	case len(parts) == 4 && parts[0] == django.ShippingProviderACS:
		return PickupLocation{Provider: parts[0], Country: parts[1],
			ID: parts[2], Branch: parts[3]}, nil
	}
	return PickupLocation{}, ErrUnknownLocation
}

// LocationOf is the pickup location a delivery names.
func LocationOf(f checkout.Fulfillment) PickupLocation {
	l := PickupLocation{Provider: f.ProviderCode, Country: f.CountryCode}
	switch f.ProviderCode {
	case django.ShippingProviderACS:
		l.ID, l.Branch = f.AcsStationExternalID, f.AcsStationBranch
	case django.ShippingProviderBoxNow:
		l.ID = f.BoxnowLockerID
	}
	return l
}

// offersPickup reports whether the store delivers the cart to pickup
// points in the country priced.
func offersPickup(p *checkout.Pricing) bool {
	for _, o := range p.ShippingOptions {
		if o.Kind == checkout.FulfillmentPickupPoint && o.FitsCart() {
			return true
		}
	}
	return false
}

// pickupPointsNear lists the ACS points near where the buyer is (the
// checkout's locality), in the country its options are priced for, when
// the store delivers the cart to ACS points there: a platform selects
// one. Without a country none is searched, so the list never holds a
// point of a country not priced. BOX NOW lockers are found only from a
// street (find_pickup_points). A lookup that fails lists none, and says
// so in a warning: the checkout is complete without the list.
func (b *Builder) pickupPointsNear(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	p *checkout.Pricing,
) ([]Destination, *Message) {
	loc := s.Locality
	country := checkout.QuoteRequest(s).CountryID
	if loc.PostalCode == "" || country == "" || s.OrderUUID != "" ||
		!offersACSPickup(p) {
		return nil, nil
	}
	stations, err := b.dj.AcsNearestStations(ctx, t.Domain,
		django.Language(ctx, t.TenantConfig), loc.PostalCode, "", country)
	if err != nil {
		// The Django client logs the failed call.
		return nil, &Message{Type: "warning", Code: "pickup_points_unlisted",
			Path: "$.fulfillment.methods[1].destinations",
			Text: "The pickup points near " + loc.PostalCode + " could " +
				"not be listed now; find them with find_pickup_points, " +
				"or read the checkout again."}
	}
	var out []Destination
	for _, st := range stations {
		if len(out) == maxNearPoints {
			break
		}
		l := PickupLocation{Provider: django.ShippingProviderACS,
			Country: strings.ToUpper(st.CountryCode), ID: st.ExternalID,
			Branch: st.BranchCode}
		out = append(out, locationDestination(l, &checkout.Point{
			Name: st.Name, Street: st.AddressLine1, City: st.City,
			PostalCode: st.PostalCode,
		}))
	}
	return out, nil
}

// offersACSPickup reports whether the store delivers the cart to ACS
// points in the country priced.
func offersACSPickup(p *checkout.Pricing) bool {
	for _, o := range p.ShippingOptions {
		if o.Kind == checkout.FulfillmentPickupPoint && o.FitsCart() &&
			o.ProviderCode == django.ShippingProviderACS {
			return true
		}
	}
	return false
}

// locationDestination is a pickup point as a business location. One the
// catalogue did not hold when it was chosen is named by its carrier and
// id: the spec requires a name, and the order is where Django decides on
// it.
func locationDestination(l PickupLocation, pt *checkout.Point) Destination {
	// A location's name is required: a point the catalogue holds without
	// one is named by its carrier and id, as one it does not hold is.
	d := Destination{Type: LocationType, ID: l.DestinationID(),
		Name: carrierTitle(l.Provider) + " " + l.ID}
	if pt == nil {
		return d
	}
	if name := strings.TrimSpace(pt.Name); name != "" {
		d.Name = name
	}
	d.Address = &PostalAddress{StreetAddress: pt.Street,
		AddressLocality: pt.City, PostalCode: pt.PostalCode,
		AddressCountry: l.Country}
	return d
}

// carrierTitle is how a carrier is named beside one of its point ids.
func carrierTitle(provider string) string {
	switch provider {
	case django.ShippingProviderACS:
		return "ACS"
	case django.ShippingProviderBoxNow:
		return "BOX NOW"
	}
	return provider
}

// renderFulfillment is the checkout's fulfillment. The shipping method
// is always listed; the pickup method where the store delivers the cart
// to pickup points in the country, or when the checkout chose it. The
// chosen method holds every line; with none chosen, the shipping method
// does. Each method's group lists the carriers that deliver the cart its
// way, as Django quotes them: home delivery the cart fits, once the
// country is known; for pickup, the network of the point selected. A
// placed order lists no options: there is nothing left to choose.
func renderFulfillment(
	s *checkout.Session, pricing *checkout.Pricing, lines []LineItem,
	near []Destination,
) (*Fulfillment, error) {
	ids := make([]string, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.ID)
	}
	f := s.Fulfillment
	shipping := FulfillmentMethod{
		ID: FulfillmentMethodID, Type: FulfillmentMethodType,
		LineItemIDs: []string{},
	}
	pickup := FulfillmentMethod{
		ID: PickupMethodID, Type: PickupMethodType, LineItemIDs: []string{},
	}
	if f.Pickup() {
		pickup.LineItemIDs = ids
	} else {
		shipping.LineItemIDs = ids
	}

	if !f.Pickup() && (f.Street != "" || f.AddressLine2 != "" ||
		f.City != "" || f.Zipcode != "" || f.CountryCode != "") {
		shipping.Destinations = []Destination{{
			Type: DestinationType, ID: DestinationID,
			StreetAddress: f.Street, ExtendedAddress: f.AddressLine2,
			AddressLocality: f.City, AddressRegion: f.RegionID,
			AddressCountry: f.CountryCode, PostalCode: f.Zipcode,
		}}
		shipping.SelectedDestinationID = DestinationID
	}
	if pricing.ShippingOptions != nil {
		g, err := group(FulfillmentGroupID, shipping.LineItemIDs, pricing,
			func(o django.QuoteShippingOption) bool {
				return o.Kind == checkout.FulfillmentHomeDelivery
			})
		if err != nil {
			return nil, err
		}
		if f.Kind == checkout.FulfillmentHomeDelivery &&
			pricing.DeliveryRefused == nil {
			selected := f.ProviderCode
			g.SelectedOptionID = &selected
		}
		shipping.Groups = []FulfillmentGroup{g}
	}

	methods := []FulfillmentMethod{shipping}
	if !f.Pickup() && !offersPickup(pricing) {
		return &Fulfillment{Methods: methods}, nil
	}
	pickup.Destinations = near
	if f.Pickup() && f.HasPoint() {
		l := LocationOf(f)
		pickup.SelectedDestinationID = l.DestinationID()
		listed := false
		for _, d := range near {
			listed = listed || d.ID == pickup.SelectedDestinationID
		}
		if !listed {
			pickup.Destinations = append([]Destination{
				locationDestination(l, f.Point)}, near...)
		}
		if pricing.ShippingOptions != nil {
			g, err := group(PickupGroupID, pickup.LineItemIDs, pricing,
				func(o django.QuoteShippingOption) bool {
					return o.Kind == checkout.FulfillmentPickupPoint &&
						o.ProviderCode == f.ProviderCode
				})
			if err != nil {
				return nil, err
			}
			if pricing.DeliveryRefused == nil {
				selected := f.ProviderCode
				g.SelectedOptionID = &selected
			}
			pickup.Groups = []FulfillmentGroup{g}
		}
	}
	return &Fulfillment{Methods: append(methods, pickup)}, nil
}

// group is a method's one group, id, of the lines the method holds (none
// for the method not chosen), its options the carriers of the options
// keep takes that the cart fits.
func group(
	id string, ids []string, pricing *checkout.Pricing,
	keep func(django.QuoteShippingOption) bool,
) (FulfillmentGroup, error) {
	g := FulfillmentGroup{
		ID: id, LineItemIDs: ids,
		Options: []FulfillmentOption{},
	}
	for _, o := range pricing.ShippingOptions {
		if !keep(o) || !o.FitsCart() {
			continue
		}
		price, err := money.MinorUnits(o.Price.String())
		if err != nil {
			return g, fmt.Errorf("option %s %s price %q: %w",
				o.ProviderCode, o.Kind, o.Price, err)
		}
		g.Options = append(g.Options, FulfillmentOption{
			ID: o.ProviderCode, Title: o.ProviderName,
			Carrier: o.ProviderName,
			Totals:  []LineItemTotal{{Type: "total", Amount: price}},
		})
	}
	return g, nil
}

// missing are the recoverable errors of what an open checkout still
// needs, each at its place in the checkout f renders. A checkout whose
// platform did not negotiate the fulfillment extension can be given no
// delivery: it is told so.
func missing(s *checkout.Session, f *Fulfillment) []Message {
	fulfills := f != nil
	var out []Message
	add := func(code, path, text string) {
		out = append(out, Message{Type: "error", Code: code, Path: path,
			Severity: "recoverable", Text: text})
	}
	for _, need := range s.Missing() {
		switch need {
		case checkout.NeedBuyer:
			add("missing", "$.buyer", "The buyer's first_name, "+
				"last_name, email and phone_number are required.")
		case checkout.NeedAddress:
			if !fulfills {
				add("missing", "$.fulfillment", "A delivery is required, "+
					"and is sent in the fulfillment extension ("+
					CapabilityFulfillment+"), which your profile does not "+
					"declare: declare it, or hand the buyer to the store.")
				continue
			}
			add("missing", pathDestinations, "A shipping destination is "+
				"required: street_address (the street and its house "+
				"number), address_locality, postal_code and "+
				"address_country; a floor or flat in extended_address. "+
				"Or choose the pickup method, where the store offers it.")
		case checkout.NeedPoint:
			add("missing", pathPoint, "Select a pickup point: the id of "+
				"one of the pickup method's destinations, or a "+
				"destinationId from find_pickup_points.")
		case checkout.NeedDestination:
			r := s.DestinationRefused
			path := pathStreet
			switch {
			case r.Code == django.CodeRegionNotServed,
				r.Code == django.CodeRegionRequired:
				path = pathRegion
			case r.Code == django.CodeCountryNotServed:
				path = pathCountry
			case s.Fulfillment.Pickup():
				path = pathPoint
			}
			add(r.Code, path, DestinationRefusedText(r.Code))
		case checkout.NeedRegion:
			add("missing", pathRegion, "address_region must name a "+
				"region of the country, one of: "+s.RegionChoices())
		case checkout.NeedDelivery:
			if !fulfills {
				continue
			}
			// The group is rendered once its options are known: until
			// then the carrier is chosen at the shipping method.
			if len(f.Methods[0].Groups) == 0 {
				add("missing", pathShipping, "Select the carrier that "+
					"delivers once the shipping group lists the options: "+
					"write the destination first, or choose the pickup "+
					"method.")
				continue
			}
			add("missing", pathOption, "Select one of the shipping "+
				"group's options, the carrier that delivers, or the "+
				"pickup method.")
		case checkout.NeedPayment:
			add("missing", "$.payment", "Select a payment method: one "+
				"of the instruments this checkout advertises.")
		}
	}
	return out
}

// DestinationRefusedText says why the store refused the destination, by
// the refusal's code.
func DestinationRefusedText(code string) string {
	switch code {
	case django.CodeStreetNumberIsPostcode:
		return "The number at the end of street_address reads as a " +
			"postal code. Send the street with its house number in " +
			"street_address (e.g. \"Ermou 12\"), and the postal code in " +
			"postal_code."
	case django.CodePickupPointInAnotherCountry:
		return "This pickup point is in another country than the one " +
			"its id names. Select a point in the delivery country: one " +
			"of the pickup method's destinations, or a destinationId " +
			"from find_pickup_points for that country."
	case django.CodePickupPointUnknown, django.CodePickupPointRequired:
		return "The store does not know this pickup point, or no longer " +
			"delivers to it. Select another: one of the pickup method's " +
			"destinations, or a destinationId from find_pickup_points."
	case django.CodeStreetNumberMissing:
		return "The store could not read a house number at the end of " +
			"street_address. Send the street followed by its house " +
			"number (e.g. \"Ermou 12\"), and a floor or flat in " +
			"extended_address."
	case django.CodeRegionNotServed:
		return "The store no longer delivers to the region in " +
			"address_region. Send the destination again with another " +
			"region of the country, or choose the pickup method, where " +
			"the store offers it."
	case django.CodeRegionRequired:
		return "The store needs the region of the destination. Send the " +
			"destination again with its region in address_region."
	case django.CodeCountryNotServed:
		return "The store delivers to no address in this country. Send a " +
			"destination in another country, or choose the pickup method, " +
			"where the store offers it."
	}
	return "The store refused the street in street_address. Send the " +
		"street's name followed by its house number (e.g. \"Ermou 12\"), " +
		"and a floor or flat in extended_address."
}
