package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// Canonical request shapes for the UCP MCP transport binding. The binding
// separates resource identity from payload: `meta` carries protocol
// metadata, `id` names the target resource, and `checkout` carries the
// domain object. Field names are the wire names from the OpenRPC document
// the profile advertises — hyphenated `ucp-agent` and snake_case payload
// members included — so a platform generating calls from that document
// reaches these tools unmodified.

// UCPAgentIn identifies the calling platform. Maps to the UCP-Agent HTTP
// header on the REST binding.
type UCPAgentIn struct {
	Profile string `json:"profile"`
}

// MetaIn is the request metadata every canonical tool carries.
type MetaIn struct {
	UCPAgent *UCPAgentIn `json:"ucp-agent"`
	// IdempotencyKey is required on complete_checkout and
	// cancel_checkout, where a retried call must not place or cancel a
	// second order.
	IdempotencyKey string `json:"idempotency-key,omitempty"`
}

// errIdempotencyKeyForm refuses a key outside checkout.ValidIdempotencyKey.
// The call did nothing, so a key of the right form may be sent.
var errIdempotencyKeyForm = fmt.Errorf(
	"meta.idempotency-key must be 1 to %d visible ASCII characters "+
		"(no spaces), such as a UUID; this call did nothing, so send it "+
		"again with a key of that form", checkout.MaxIdempotencyKeyLen)

// validate enforces the binding's metadata rules. needIdempotency is set
// for the two operations the spec singles out for retry safety; their key
// is checked here, before anything is read or written. The profile URL
// itself is vetted when it is resolved (negotiate), where the
// fetch-safety rules live.
func (m *MetaIn) validate(needIdempotency bool) error {
	if m == nil || m.UCPAgent == nil || m.UCPAgent.Profile == "" {
		return errors.New(
			"meta.ucp-agent.profile is required: it identifies the " +
				"calling platform so capabilities can be negotiated")
	}
	if !needIdempotency {
		return nil
	}
	if m.IdempotencyKey == "" {
		return errors.New(
			"meta.idempotency-key is required on this operation so a " +
				"retry cannot place or cancel a second order")
	}
	if !checkout.ValidIdempotencyKey(m.IdempotencyKey) {
		return errIdempotencyKeyForm
	}
	return nil
}

// UCPItemIn identifies a product. Only `id` travels on a request — the
// business owns title, pricing and imagery, so the item schema omits
// them in the request direction.
type UCPItemIn struct {
	ID string `json:"id"`
}

// UCPLineItemReqIn is one requested line. The business assigns line ids,
// and a line's product names it, so a line's `id` is not read.
type UCPLineItemReqIn struct {
	Item     UCPItemIn `json:"item"`
	Quantity int       `json:"quantity"`
}

// UCPBuyerReqIn carries the buyer's contact details.
type UCPBuyerReqIn struct {
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
	Email       string `json:"email"`
	PhoneNumber string `json:"phone_number"`
}

// UCPInstrumentIn is a submitted payment instrument. Its `type` must be
// one the authoritative response advertised for this checkout, and
// `handler_id` must match the handler that advertised it. No credential
// member is read: the only instrument this business accepts forbids one.
type UCPInstrumentIn struct {
	HandlerID string `json:"handler_id"`
	Type      string `json:"type"`
	Selected  bool   `json:"selected"`
}

// UCPPaymentIn is the payment object, required when completing.
type UCPPaymentIn struct {
	Instruments []UCPInstrumentIn `json:"instruments"`
}

// UCPDiscountsIn is the discount extension's member: the codes to apply,
// replacing those submitted before. The store applies one code per
// order; extra codes come back rejected in `messages`.
type UCPDiscountsIn struct {
	Codes []string `json:"codes"`
}

// UCPCheckoutIn is the `checkout` of a create or an update, decoded from
// what the spec's request schema accepts (ucp_inputs.go). Extensions
// compose FLAT onto the checkout object rather than nesting under their
// capability names: `discounts` is the discount extension's, read only
// while that extension is negotiated.
type UCPCheckoutIn struct {
	LineItems []UCPLineItemReqIn `json:"line_items"`
	Buyer     *UCPBuyerReqIn     `json:"buyer"`
	// Fulfillment is the gateway's own delivery shape, not the spec's
	// fulfillment extension, which this business does not implement yet:
	// it is accepted, and described in the published schema as the
	// business's own (ownFulfillment), until that extension replaces it.
	// It is read only when it names its `kind`; the extension's own
	// member (`methods`) is ignored, as a member of an extension that
	// was not negotiated.
	Fulfillment *checkout.Fulfillment `json:"fulfillment"`
	Discounts   *UCPDiscountsIn       `json:"discounts"`
	UCPPaymentChoice
}

// UCPPaymentChoice is how a request selects its payment method, and all a
// complete_checkout reads of its `checkout`: the spec omits every other
// member from the complete request.
type UCPPaymentChoice struct {
	Payment *UCPPaymentIn `json:"payment"`
	// PayWayID selects a payment method UCP cannot currently name.
	//
	// `payment.instruments` can only carry a type the checkout
	// advertised, and this business advertises only what an agent can
	// settle itself — cash on delivery. An ONLINE method has no
	// instrument type, because settling it means sending the buyer to
	// the PSP's own page, which UCP models as an escalation rather than
	// as an instrument. Until that option is modelled as an instrument
	// in its own right, an agent choosing card payment has no canonical
	// way to say so, and this member is how it does.
	//
	// It is the member of the business's declared extension
	// space.grooveshop.payments.hosted_selection, described in the
	// published schema beside the spec's members. Unlike other members
	// of an extension that was not negotiated, it is refused rather than
	// ignored (applyHostedSelection).
	PayWayID int64 `json:"pay_way_id"`
}

// buyer converts the request buyer into the session shape: none when
// the request carries none.
func (c *UCPCheckoutIn) buyer() checkout.Buyer {
	if c == nil || c.Buyer == nil {
		return checkout.Buyer{}
	}
	return checkout.Buyer{
		FirstName: c.Buyer.FirstName,
		LastName:  c.Buyer.LastName,
		Email:     c.Buyer.Email,
		Phone:     c.Buyer.PhoneNumber,
	}
}

// fulfillment is the gateway's own delivery member, when the request
// carries one (see UCPCheckoutIn.Fulfillment).
func (c *UCPCheckoutIn) fulfillment() *checkout.Fulfillment {
	if c == nil || c.Fulfillment == nil || c.Fulfillment.Kind == "" {
		return nil
	}
	return c.Fulfillment
}

// lines flattens the requested line items into product lines. Item ids
// are strings on the wire and integers in the catalog.
func (c *UCPCheckoutIn) lines() ([]checkout.Line, error) {
	if c == nil {
		return nil, nil
	}
	out := make([]checkout.Line, 0, len(c.LineItems))
	for i, li := range c.LineItems {
		id, err := strconv.ParseInt(li.Item.ID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf(
				"checkout.line_items[%d].item.id %q is not a product id",
				i, li.Item.ID)
		}
		out = append(out,
			checkout.Line{ProductID: id, Quantity: li.Quantity})
	}
	return out, nil
}

// resolvePayWay maps a submitted instrument onto the merchant pay-way
// that settles it. The instrument type is the protocol's vocabulary; the
// pay-way id is the store's, and only the business can bridge them.
//
// A type the store cannot settle is rejected rather than silently
// ignored: completing against an unhonourable instrument would place an
// order the buyer never authorised a way to pay for.
//
// The candidates are the methods offered for the session's delivery, so
// an instrument the chosen carrier cannot settle is refused here rather
// than by Django at order creation.
func resolvePayWay(
	ctx context.Context, dj *django.Client, t *tenant.Tenant,
	f checkout.Fulfillment, instruments []UCPInstrumentIn,
) (int64, error) {
	// Honour the platform's selection when it marks one, else the first.
	chosen := instruments[0]
	for _, in := range instruments {
		if in.Selected {
			chosen = in
			break
		}
	}
	if chosen.HandlerID != "" && chosen.HandlerID != ucp.HandlerID {
		return 0, fmt.Errorf(
			"unknown payment handler_id %q; this store advertises %q",
			chosen.HandlerID, ucp.HandlerID)
	}

	pws, err := checkout.PayWaysFor(ctx, dj, t, f)
	if err != nil {
		return 0, upstreamErr(err, "payment methods are unavailable")
	}
	for i := range pws {
		pw := &pws[i]
		// Positive test, not "not online". An ONLINE pay way is not an
		// agent-payable instrument (it needs the store's hosted flow),
		// and neither is one whose settlement we cannot read — the
		// profile fails CLOSED by design, so an absent field must
		// withhold the instrument rather than advertise it.
		if !pw.Active || !pw.IsCollectedLater() || pw.ProviderCode == "" {
			continue
		}
		if kind, ok := ucp.InstrumentTypeFor(pw.ProviderCode); ok &&
			kind == chosen.Type {
			return pw.ID, nil
		}
	}
	return 0, fmt.Errorf(
		"payment instrument type %q is not available for this checkout",
		chosen.Type)
}

// discountCodes reports the submitted codes, and whether the request
// names any: `discounts.codes`, read only while the discount extension
// is negotiated. An empty array names none, and clears them.
func (c *UCPCheckoutIn) discountCodes(neg ucp.Negotiated) ([]string, bool) {
	if c == nil || c.Discounts == nil || c.Discounts.Codes == nil ||
		!neg.Has(ucp.CapabilityDiscount) {
		return nil, false
	}
	return c.Discounts.Codes, true
}

// replacedDiscountCodes is discountCodes for an update, which replaces the
// checkout: with the extension negotiated, a request without codes holds
// none, and the cart's coupon is cleared. A checkout's codes are its
// cart's, a coupon the cart held before the checkout included
// (ucp.renderDiscounts lists it), so the clear does not depend on what
// this checkout submitted; the store's removal changes nothing on a cart
// without one.
func (c *UCPCheckoutIn) replacedDiscountCodes(
	neg ucp.Negotiated,
) ([]string, bool) {
	codes, ok := c.discountCodes(neg)
	if !ok && neg.Has(ucp.CapabilityDiscount) {
		return []string{}, true
	}
	return codes, ok
}

// applyTo copies the submitted buyer and fulfillment onto the session.
// An update replaces the checkout, so the buyer is what the request
// carries, none included. The gateway's own fulfillment member is not
// in the response a platform sends back, so one the request omits keeps
// the stored delivery.
//
// Payment is deliberately NOT applied here: resolving an instrument to a
// pay-way needs the merchant's catalogue, so it happens where a client
// is available.
func (c *UCPCheckoutIn) applyTo(s *checkout.Session) {
	s.Buyer = c.buyer()
	if f := c.fulfillment(); f != nil {
		s.Fulfillment = *f
	}
}

// applyHostedSelection honours a submitted pay-way id, refusing it unless
// the hosted-selection extension is negotiated for this request — which
// needs the tenant's gate on (the business declares it only then) and
// the platform declaring it too.
//
// Refusing beats ignoring: a platform that named a method and got a
// silent no-op would complete against whatever was selected before, and
// the buyer could be charged a different way than the agent chose.
func (c *UCPPaymentChoice) applyHostedSelection(
	s *checkout.Session, neg ucp.Negotiated,
) error {
	if c == nil || c.PayWayID <= 0 {
		return nil
	}
	if c.Payment != nil && len(c.Payment.Instruments) > 0 {
		return errors.New(
			"send either checkout.pay_way_id or checkout.payment." +
				"instruments, not both: they select competing payment " +
				"methods")
	}
	if !neg.Has(ucp.HostedSelectionCapability) {
		return fmt.Errorf(
			"checkout.pay_way_id is not accepted: the %s capability is "+
				"not negotiated (this store may not offer it, or your "+
				"profile does not declare it). Submit one of the "+
				"instruments the checkout advertises, or hand the buyer "+
				"to continue_url", ucp.HostedSelectionCapability)
	}
	s.PayWayID = c.PayWayID
	return nil
}
