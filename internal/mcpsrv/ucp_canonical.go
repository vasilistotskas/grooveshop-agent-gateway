package mcpsrv

import (
	"context"
	"fmt"
	"strconv"
	"strings"

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
	Profile string `json:"profile" jsonschema:"URL of the calling platform's UCP profile document"`
}

// MetaIn is the request metadata every canonical tool carries.
type MetaIn struct {
	UCPAgent *UCPAgentIn `json:"ucp-agent" jsonschema:"REQUIRED; identifies the calling platform for capability negotiation"`
	// IdempotencyKey is required on complete_checkout and
	// cancel_checkout, where a retried call must not place or cancel a
	// second order.
	IdempotencyKey string `json:"idempotency-key,omitempty" jsonschema:"UUID; REQUIRED on complete_checkout and cancel_checkout, repeat it to retry safely"`
}

// validate enforces the binding's metadata rules. needIdempotency is set
// for the two operations the spec singles out for retry safety.
func (m *MetaIn) validate(needIdempotency bool) error {
	if m == nil || m.UCPAgent == nil || m.UCPAgent.Profile == "" {
		return fmt.Errorf(
			"meta.ucp-agent.profile is required: it identifies the " +
				"calling platform so capabilities can be negotiated")
	}
	if !strings.HasPrefix(m.UCPAgent.Profile, "https://") {
		return fmt.Errorf(
			"meta.ucp-agent.profile must be an https URL, got %q",
			m.UCPAgent.Profile)
	}
	if needIdempotency && m.IdempotencyKey == "" {
		return fmt.Errorf(
			"meta.idempotency-key is required on this operation so a " +
				"retry cannot place or cancel a second order")
	}
	return nil
}

// UCPItemIn identifies a product. Only `id` travels on a request — the
// business owns title, pricing and imagery, so the item schema omits
// them in the request direction.
type UCPItemIn struct {
	ID string `json:"id" jsonschema:"the product id from the catalog tools"`
}

// UCPLineItemReqIn is one requested line. `id` and `totals` are omitted
// on create (the business assigns them); `id` addresses an existing line
// on update.
type UCPLineItemReqIn struct {
	ID       string    `json:"id,omitempty" jsonschema:"existing line id; omit when creating"`
	Item     UCPItemIn `json:"item"`
	Quantity int       `json:"quantity"`
}

// UCPBuyerReqIn carries the buyer's contact details.
type UCPBuyerReqIn struct {
	FirstName   string `json:"first_name,omitempty"`
	LastName    string `json:"last_name,omitempty"`
	Email       string `json:"email,omitempty"`
	PhoneNumber string `json:"phone_number,omitempty"`
}

// UCPInstrumentIn is a submitted payment instrument. Its `type` must be
// one the authoritative response advertised for this checkout, and
// `handler_id` must match the handler that advertised it. No credential
// member exists: the only instrument this business accepts forbids one.
type UCPInstrumentIn struct {
	ID        string `json:"id,omitempty"`
	HandlerID string `json:"handler_id" jsonschema:"the id of the payment handler that advertised this instrument"`
	Type      string `json:"type" jsonschema:"instrument type from the checkout's available_instruments"`
	Selected  bool   `json:"selected,omitempty"`
}

// UCPPaymentIn is the payment object, required when completing.
type UCPPaymentIn struct {
	Instruments []UCPInstrumentIn `json:"instruments,omitempty"`
}

// UCPDiscountsIn carries submitted discount codes. The store applies one
// code per order; extra codes come back rejected in `messages`.
type UCPDiscountsIn struct {
	Codes []string `json:"codes,omitempty"`
}

// UCPCheckoutIn is the `checkout` request payload. `fulfillment` and
// `discounts` are contributed by the fulfillment and discount
// extensions, which compose FLAT onto the checkout object rather than
// nesting under their capability names.
type UCPCheckoutIn struct {
	LineItems   []UCPLineItemReqIn    `json:"line_items,omitempty"`
	Buyer       *UCPBuyerReqIn        `json:"buyer,omitempty"`
	Fulfillment *checkout.Fulfillment `json:"fulfillment,omitempty" jsonschema:"delivery details; kind home_delivery or pickup_point with providerCode acs/boxnow"`
	Discounts   *UCPDiscountsIn       `json:"discounts,omitempty"`
	Payment     *UCPPaymentIn         `json:"payment,omitempty"`
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
	// A canonical caller never sends it; consumers ignore members they
	// do not recognise.
	PayWayID int64 `json:"pay_way_id,omitempty" jsonschema:"selects an ONLINE payment method (see get_payment_methods); omit and use payment.instruments for methods the checkout advertises"`
}

// buyer converts the request buyer into the session shape, reporting
// whether anything was supplied.
func (c *UCPCheckoutIn) buyer() (checkout.Buyer, bool) {
	if c == nil || c.Buyer == nil {
		return checkout.Buyer{}, false
	}
	return checkout.Buyer{
		FirstName: c.Buyer.FirstName,
		LastName:  c.Buyer.LastName,
		Email:     c.Buyer.Email,
		Phone:     c.Buyer.PhoneNumber,
	}, true
}

// productQuantities flattens the requested lines into product/quantity
// pairs. Item ids are strings on the wire and integers in the catalog.
func (c *UCPCheckoutIn) productQuantities() ([]productQuantity, error) {
	if c == nil {
		return nil, nil
	}
	out := make([]productQuantity, 0, len(c.LineItems))
	for i, li := range c.LineItems {
		id, err := strconv.ParseInt(li.Item.ID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf(
				"checkout.line_items[%d].item.id %q is not a product id",
				i, li.Item.ID)
		}
		qty := li.Quantity
		if qty <= 0 {
			qty = 1
		}
		out = append(out, productQuantity{ProductID: id, Quantity: qty})
	}
	return out, nil
}

// productQuantity is one requested product line.
type productQuantity struct {
	ProductID int64
	Quantity  int
}

// resolvePayWay maps a submitted instrument onto the merchant pay-way
// that settles it. The instrument type is the protocol's vocabulary; the
// pay-way id is the store's, and only the business can bridge them.
//
// A type the store cannot settle is rejected rather than silently
// ignored: completing against an unhonourable instrument would place an
// order the buyer never authorised a way to pay for.
func resolvePayWay(
	ctx context.Context, dj *django.Client, t *tenant.Tenant,
	payment *UCPPaymentIn,
) (int64, error) {
	if payment == nil || len(payment.Instruments) == 0 {
		return 0, fmt.Errorf(
			"checkout.payment.instruments is required to complete: " +
				"submit one of the instruments the checkout advertised")
	}

	// Honour the platform's selection when it marks one, else the first.
	chosen := payment.Instruments[0]
	for _, in := range payment.Instruments {
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

	page, err := dj.PayWays(ctx, t.Domain, t.DefaultLocale, "", "")
	if err != nil {
		return 0, fmt.Errorf("pay ways unavailable: %w", err)
	}
	for i := range page.Results {
		pw := &page.Results[i]
		if !pw.Active || pw.IsOnlinePayment || pw.ProviderCode == "" {
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

// discountCodes reports the submitted codes and whether the member was
// present at all. Absent means "leave the coupon alone"; an empty array
// means "remove it", so the two cannot be collapsed.
func (c *UCPCheckoutIn) discountCodes() ([]string, bool) {
	if c == nil || c.Discounts == nil {
		return nil, false
	}
	if c.Discounts.Codes == nil {
		return []string{}, true
	}
	return c.Discounts.Codes, true
}

// applyTo copies the submitted buyer and fulfillment onto the session.
// Absent members leave the stored value untouched, so a platform can
// update one field without resending the rest.
//
// Payment is deliberately NOT applied here: resolving an instrument to a
// pay-way needs the merchant's catalogue, so it happens where a client
// is available.
func (c *UCPCheckoutIn) applyTo(s *checkout.Session) {
	if c == nil {
		return
	}
	if b, ok := c.buyer(); ok {
		s.Buyer = b
	}
	if c.Fulfillment != nil {
		s.Fulfillment = *c.Fulfillment
	}
}

// applyHostedSelection honours a submitted pay-way id, refusing it when
// the tenant's hosted-payment gate is off.
//
// Refusing beats ignoring: a platform that named a method and got a
// silent no-op would complete against whatever was selected before, and
// the buyer could be charged a different way than the agent chose.
func (c *UCPCheckoutIn) applyHostedSelection(
	t *tenant.Tenant, s *checkout.Session,
) error {
	if c == nil || c.PayWayID <= 0 {
		return nil
	}
	if !t.HostedPaymentOn() {
		return fmt.Errorf(
			"checkout.pay_way_id is not accepted by this store: it "+
				"advertises no %s capability. Submit one of the "+
				"instruments the checkout advertises, or hand the buyer "+
				"to continue_url", ucp.HostedSelectionCapability)
	}
	s.PayWayID = c.PayWayID
	return nil
}
