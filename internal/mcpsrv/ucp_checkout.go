package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcptool"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// The UCP checkout tools return the spec's checkout session object as
// structuredContent (the MCP transport binding requirement). Inputs are
// MCP-self-describing: platforms read the input schema from tools/list.

// CreateCheckoutIn is the create_checkout tool's arguments.
type CreateCheckoutIn struct {
	Meta     *MetaIn       `json:"meta"`
	Checkout UCPCheckoutIn `json:"checkout"`
	// CartID starts the checkout from a cart built with the cart tools.
	// UCP models that in its cart capability (dev.ucp.shopping.cart),
	// as a cart_id member of the checkout object. This business does
	// not advertise that capability, so a canonical caller sends
	// line_items, and the member stays here, beside `checkout`, outside
	// the negotiated shape: consumers ignore members they do not
	// recognise. The capability's aim is kept, a cart has one
	// incomplete checkout, but not its means: a checkout is not bound
	// to its platform yet, so the spec's "return the existing checkout"
	// is not offered. The newest create holds the cart, and an open
	// checkout on it is canceled (createFromCart). (Order webhooks are
	// discovered from the platform profile, never taken from a request.)
	CartID string `json:"cart_id,omitempty" jsonschema:"the cartId of an existing cart from the cart tools; omit when sending line_items. A cart has one checkout at a time: an open checkout that already holds the cart is canceled, and this one replaces it"`
}

// GetCheckoutIn is the get_checkout tool's arguments.
type GetCheckoutIn struct {
	Meta *MetaIn `json:"meta"`
	ID   string  `json:"id" jsonschema:"the checkout session id"`
}

// UpdateCheckoutIn is the update_checkout tool's arguments.
type UpdateCheckoutIn struct {
	Meta     *MetaIn       `json:"meta"`
	ID       string        `json:"id" jsonschema:"the checkout session id to update"`
	Checkout UCPCheckoutIn `json:"checkout"`
}

// CompleteCheckoutIn is the complete_checkout tool's arguments.
type CompleteCheckoutIn struct {
	Meta *MetaIn `json:"meta"`
	ID   string  `json:"id" jsonschema:"the checkout session id to place"`
	// Checkout carries the payment object the business needs to settle.
	Checkout UCPCheckoutIn `json:"checkout"`
}

// CancelCheckoutIn is the cancel_checkout tool's arguments.
type CancelCheckoutIn struct {
	Meta *MetaIn `json:"meta"`
	ID   string  `json:"id" jsonschema:"the checkout session id to cancel"`
}

func (h *handlers) createCheckout(
	ctx context.Context, _ *mcp.CallToolRequest, in CreateCheckoutIn,
) (*mcp.CallToolResult, CheckoutResult, error) {
	var zero CheckoutResult
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	if err := in.Meta.validate(false); err != nil {
		return nil, zero, err
	}
	neg, err := h.negotiate(ctx, t, in.Meta, h.linksFor(ctx, t).Cart())
	if err != nil {
		return nil, zero, err
	}
	if !neg.Has(ucp.CapabilityCheckout) {
		return nil, incompatibleCheckout(h.linksFor(ctx, t)), nil
	}
	lines, err := in.Checkout.lines()
	if err != nil {
		return nil, zero, err
	}
	switch {
	case in.CartID != "" && len(lines) > 0:
		return nil, zero, errors.New(
			"send either cart_id or checkout.line_items, not both: the " +
				"cart already holds its lines")
	case in.CartID == "" && len(lines) == 0:
		return nil, zero, errors.New(
			"checkout.line_items is required to start a checkout")
	case in.CartID != "":
		return h.createFromCart(ctx, t, in, neg)
	}

	// Every lookup and refusal before the first cart write, which here
	// is creating the cart: a region or payment the store refuses leaves
	// no cart behind. A line sync failing after it does.
	s, err := h.newSession(ctx, t, &in.Checkout, neg)
	if err != nil {
		return nil, zero, err
	}
	c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, "")
	if err != nil {
		return nil, zero, upstreamErr(err, "the cart service is unavailable")
	}
	s.CartID = c.UUID
	if err := checkout.SyncCartLines(
		ctx, h.deps.Django, t, checkout.ProtocolUCP, s.CartID, lines,
	); err != nil {
		return nil, zero, withHint(upstreamErr(err,
			"the cart made for this checkout could not be reached; "+
				"start again"), checkoutNotCreated)
	}
	hasCodes, err := h.applyCodes(ctx, t, s, &in.Checkout)
	if err != nil {
		return nil, zero, err
	}
	s.Recompute()
	// The cart was made for this checkout, so no checkout holds it.
	if err := h.deps.Checkout.Create(
		ctx, s, checkout.CartHolder{},
	); err != nil {
		if errors.Is(err, checkout.ErrCartHeld) {
			h.deps.Log.ErrorContext(ctx,
				"ucp: a cart just made is already in a checkout")
		}
		return nil, zero, errCheckoutUnavailable
	}
	res, out, err := h.checkoutResult(ctx, t, s, neg)
	return h.discountAnnotated(res, out, err, s, hasCodes)
}

// newSession is the session a create_checkout would store, with every
// lookup and refusal settled and nothing written: it has no cart yet.
func (h *handlers) newSession(
	ctx context.Context, t *tenant.Tenant, in *UCPCheckoutIn,
	neg ucp.Negotiated,
) (*checkout.Session, error) {
	s := checkout.NewSession(
		t.SchemaName, t.Domain, checkout.ProtocolUCP, "")
	// The platform's order-event endpoint, from its profile's negotiated
	// order capability (vetted by negotiate); none when it negotiated
	// no order capability.
	s.WebhookURL = neg.OrderWebhookURL()
	in.applyTo(s)
	if err := h.applyRegion(ctx, t, s, in); err != nil {
		return nil, err
	}
	if err := h.applyPayment(ctx, t, s, in, neg); err != nil {
		return nil, err
	}
	return s, nil
}

// applyCodes writes a create's discount codes to its cart, and reports
// whether it sent any.
func (h *handlers) applyCodes(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	in *UCPCheckoutIn,
) (bool, error) {
	codes, hasCodes := in.discountCodes()
	if !hasCodes {
		return false, nil
	}
	if err := checkout.ApplyDiscountCodes(
		ctx, h.deps.Django, t, s, codes,
	); err != nil {
		return true, withHint(upstreamErr(err,
			"the cart was not found; start a new checkout with "+
				"line_items"), checkoutNotCreated)
	}
	return true, nil
}

// cartCreateRounds bounds how often a create naming a cart decides
// again about the cart's holder, after another request changed it.
const cartCreateRounds = 3

// createFromCart is create_checkout for a cart the caller names. A cart
// has one live checkout, which the store keeps (checkout.Store.Create):
// two checkouts on one cart could each place an order, because Django
// keeps the cart of an order that awaits its online payment.
//
// The newest create holds the cart. An open checkout on it is canceled,
// with cancel_checkout's own write, and this call's checkout takes its
// place: the old id reads canceled from then on. UCP's cart capability
// would return the open checkout instead; a checkout is not bound to
// its platform yet, so that is not offered, and no answer here gives
// another checkout or names one.
//
// A checkout whose order is being placed or awaits its payment keeps
// the cart, and the create is refused (cartHolder), before any lookup
// and any cart write. What the create submits is settled before the
// holder is canceled: a create the store refuses cancels nothing.
func (h *handlers) createFromCart(
	ctx context.Context, t *tenant.Tenant, in CreateCheckoutIn,
	neg ucp.Negotiated,
) (*mcp.CallToolResult, CheckoutResult, error) {
	var zero CheckoutResult
	// Django reads every spelling of a UUID as one cart, so the cart is
	// named in one spelling here: it keys the cart's record.
	cartID, ok := checkout.CanonicalCartID(in.CartID)
	if !ok {
		return nil, zero, errors.New(
			"cart_id is not a cart id: send the cartId the cart tools " +
				"returned, or omit it and send checkout.line_items")
	}
	if _, err := h.cartHolder(ctx, t, cartID); err != nil {
		return nil, zero, err
	}
	s, err := h.newSession(ctx, t, &in.Checkout, neg)
	if err != nil {
		return nil, zero, err
	}
	s.CartID = cartID

	// The cart is written once, however many rounds follow. hasCodes then
	// says this call's discount codes are on the cart.
	hasCodes, wrote := false, false
	for range cartCreateRounds {
		// Read again: the lookups above take time, and each further
		// round follows another request's change to the holder.
		holder, err := h.cartHolder(ctx, t, cartID)
		if err != nil {
			return nil, zero, afterCodes(err, hasCodes)
		}
		if !holder.Free() {
			err = h.cancel(ctx, holder.Session)
			if errors.Is(err, checkout.ErrSessionChanged) {
				// The holder moved on, perhaps into placing its order.
				continue
			}
			if err != nil {
				return nil, zero, afterCodes(errCheckoutUnavailable, hasCodes)
			}
			h.deps.Log.InfoContext(ctx,
				"ucp: a create canceled the open checkout on its cart",
				slog.String("session", holder.Session.ID))
		}
		// The first cart write, once no live checkout holds the cart.
		if !wrote {
			hasCodes, err = h.applyCodes(ctx, t, s, &in.Checkout)
			if err != nil {
				return nil, zero, err
			}
			wrote = true
			s.Recompute()
		}
		err = h.deps.Checkout.Create(ctx, s, holder)
		if err == nil {
			res, out, err := h.checkoutResult(ctx, t, s, neg)
			return h.discountAnnotated(res, out, err, s, hasCodes)
		}
		if !errors.Is(err, checkout.ErrCartHeld) {
			return nil, zero, afterCodes(errCheckoutUnavailable, hasCodes)
		}
		// Another create took the cart since it was read.
	}
	return nil, zero, afterCodes(errCartBusy, hasCodes)
}

// afterCodes adds, to a create's refusal, that its discount codes went
// to the cart before it was refused.
func afterCodes(err error, written bool) error {
	if !written {
		return err
	}
	return withHint(err,
		"The discount codes this call sent may already be on the cart")
}

// cancel stores an open checkout as canceled, and only over the
// checkout s was loaded as (Store.Save): what cancel_checkout does to
// its checkout, and what a create does to the open checkout whose cart
// it takes.
func (h *handlers) cancel(ctx context.Context, s *checkout.Session) error {
	s.Status = checkout.StatusCanceled
	return h.deps.Checkout.Save(ctx, s)
}

// cartHolder reads the checkout that holds a cart, for a create_checkout
// that names the cart. The holder it returns is free
// (checkout.CartHolder.Free), or is an open UCP checkout, which the
// create cancels. A holder that keeps the cart is the create's refusal:
//   - a checkout whose order is being placed or awaits its payment: an
//     order exists, or may, and a second checkout could place a second
//     one;
//   - an open checkout of another protocol, which is that surface's to
//     cancel.
//
// No refusal names the holder or its order.
func (h *handlers) cartHolder(
	ctx context.Context, t *tenant.Tenant, cartID string,
) (checkout.CartHolder, error) {
	holder, err := h.deps.Checkout.HolderOf(ctx, t.SchemaName, cartID)
	if err != nil {
		return holder, errCheckoutUnavailable
	}
	s := holder.Session
	switch {
	case holder.Free():
	case s.MayHaveOrder():
		return holder, cartOrdered(s)
	case s.Protocol != checkout.ProtocolUCP:
		return holder, errCartElsewhere
	}
	return holder, nil
}

// cartOrdered answers a create_checkout naming a cart whose checkout
// may have an order. Like errCheckoutGone it never says to start
// another checkout, and it tells the caller where to look with what it
// already holds: the checkout's id, the order's, or neither.
func cartOrdered(s *checkout.Session) error {
	if s.Status == checkout.StatusCompleteInProgress {
		return errCartOrderPending
	}
	return errCartOrderUnpaid
}

var (
	// errCartOrderPending: the cart's checkout is placing its order, or
	// sent it and was never answered.
	errCartOrderPending = &mcptool.ToolError{Code: mcptool.CodeConflict,
		Hint: "this cart already has a checkout whose order is being " +
			"placed, or was sent and never confirmed, so an order " +
			"exists or may, and no checkout was created: do not start " +
			"another checkout for it before checking. If you hold that " +
			"checkout's id, read it with get_checkout; if you hold its " +
			"order id, read the order with get_order or track_order; if " +
			"not, ask the shopper for the store's order confirmation"}

	// errCartOrderUnpaid: the cart's checkout placed an order that
	// awaits its payment.
	errCartOrderUnpaid = &mcptool.ToolError{Code: mcptool.CodeConflict,
		Hint: "this cart already has a checkout that placed an order, " +
			"which awaits its payment, so an order exists and no " +
			"checkout was created: do not start another checkout for " +
			"it. If you hold that checkout's id, read it with " +
			"get_checkout for the payment page; if you hold its order " +
			"id, read the order with get_order or track_order; if not, " +
			"ask the shopper for the store's order confirmation"}

	// errCartElsewhere: an open checkout of another protocol holds the
	// cart.
	errCartElsewhere = &mcptool.ToolError{Code: mcptool.CodeConflict,
		Hint: "this cart is already in an open checkout that was " +
			"started outside this server and cannot be changed here, " +
			"so no checkout was created; that checkout must be finished " +
			"or canceled where it was started before this cart can be " +
			"checked out here"}

	// errCartBusy: the create kept finding the cart's holder changed by
	// other requests.
	errCartBusy = &mcptool.ToolError{Code: mcptool.CodeConflict,
		Hint: "other requests are starting a checkout for this cart, so " +
			"this call created none; send it again shortly"}
)

// applyRegion settles the delivery region whenever a fulfillment was
// submitted: its country decides whether one is needed at all.
func (h *handlers) applyRegion(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	in *UCPCheckoutIn,
) error {
	if in.Fulfillment == nil {
		return nil
	}
	if err := checkout.ResolveRegion(
		ctx, h.deps.Django, t, s, in.Fulfillment.RegionID,
	); err != nil {
		return upstreamErr(err,
			"the delivery regions are unavailable; retry shortly")
	}
	return nil
}

// applyPayment honours a payment selection submitted before completion:
// it changes the totals through the method's fee. A hosted pay_way_id
// and an advertised instrument are mutually exclusive.
func (h *handlers) applyPayment(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	in *UCPCheckoutIn, neg ucp.Negotiated,
) error {
	if err := in.applyHostedSelection(s, neg); err != nil {
		return err
	}
	if in.Payment == nil {
		return nil
	}
	payWayID, err := resolvePayWay(
		ctx, h.deps.Django, t, s.Fulfillment, in.Payment)
	if err != nil {
		return err
	}
	s.PayWayID = payWayID
	return nil
}

// toolResult is a checkout tool's answer, made when it is given.
type toolResult func() (*mcp.CallToolResult, CheckoutResult, error)

// sessionAnswer is how the body of update_checkout, complete_checkout or
// cancel_checkout ends. A body returns one and nothing else, and only
// finish turns it into the tool's answer. That is what keeps every way
// out of those tools behind the check finish makes, a way added later
// included.
//
// What a body holds is a snapshot of the checkout. The lock meant to
// keep it current can lapse while Django is slow, and another call may
// have placed the order since: Django then refuses this call for reasons
// that are the other call's doing (an empty cart, a line that is gone,
// stock the order took), and the snapshot describes a checkout that no
// longer exists.
type sessionAnswer struct {
	answer toolResult
	// current: the call's own save of the checkout was just accepted,
	// which proves its snapshot was the stored checkout.
	current bool
}

// fromSnapshot is an answer made from the checkout as the call holds it.
// finish gives it only while the stored checkout is still that one.
func fromSnapshot(answer toolResult) sessionAnswer {
	return sessionAnswer{answer: answer}
}

// refusedBy is fromSnapshot for a refusal: one decided from the
// snapshot, or one Django made on its behalf.
func refusedBy(err error) sessionAnswer {
	return fromSnapshot(func() (*mcp.CallToolResult, CheckoutResult, error) {
		return nil, CheckoutResult{}, err
	})
}

// afterSave is an answer to a call whose save of the checkout was
// accepted: an update or cancel that stored, a completion past the
// in-progress save, whose outcome is its own.
func afterSave(answer toolResult) sessionAnswer {
	return sessionAnswer{answer: answer, current: true}
}

// finish gives a body's answer. One made from the snapshot stands only
// if the stored checkout is still the revision the call holds. Otherwise
// the call is answered by what is stored: stored (the operation's own
// answer for a checkout in that state, or that another request changed
// it), no longer stored (errCheckoutGone), unreadable
// (errCheckoutUnavailable).
func (h *handlers) finish(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	a sessionAnswer, stored func(now *checkout.Session) toolResult,
) (*mcp.CallToolResult, CheckoutResult, error) {
	if a.current {
		return a.answer()
	}
	var zero CheckoutResult
	now, err := h.deps.Checkout.Load(ctx, t.SchemaName, s.ID)
	switch {
	case errors.Is(err, checkout.ErrNotFound):
		return nil, zero, errCheckoutGone
	case err != nil:
		return nil, zero, errCheckoutUnavailable
	case now.Revision == s.Revision:
		return a.answer()
	}
	if answer := stored(now); answer != nil {
		return answer()
	}
	return nil, zero, errCheckoutChanged
}

func (h *handlers) updateCheckout(
	ctx context.Context, _ *mcp.CallToolRequest, in UpdateCheckoutIn,
) (*mcp.CallToolResult, CheckoutResult, error) {
	var zero CheckoutResult
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	if err := in.Meta.validate(false); err != nil {
		return nil, zero, err
	}
	neg, err := h.negotiate(ctx, t, in.Meta, h.linksFor(ctx, t).Cart())
	if err != nil {
		return nil, zero, err
	}
	if !neg.Has(ucp.CapabilityCheckout) {
		return nil, incompatibleCheckout(h.linksFor(ctx, t)), nil
	}
	s, release, err := h.lockedSession(ctx, t, in.ID)
	if err != nil {
		return nil, zero, err
	}
	defer release()
	return h.finish(ctx, t, s, h.updateSession(ctx, t, s, in, neg),
		func(now *checkout.Session) toolResult {
			if err := h.closedTo(ctx, now, "updated"); err != nil {
				return refusedBy(err).answer
			}
			// Still open: this update's cart writes may be made.
			return refusedBy(
				withHint(errCheckoutChanged, checkoutNotUpdated)).answer
		})
}

// updateSession is update_checkout once its checkout is loaded.
func (h *handlers) updateSession(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	in UpdateCheckoutIn, neg ucp.Negotiated,
) sessionAnswer {
	if err := h.closedTo(ctx, s, "updated"); err != nil {
		return refusedBy(err)
	}
	// line_items is full-state on update: the submitted list replaces
	// the cart's lines. Ignoring it would complete the old order.
	lines, err := in.Checkout.lines()
	if err != nil {
		return refusedBy(err)
	}
	if in.Checkout.LineItems != nil && len(lines) == 0 {
		return refusedBy(errors.New(
			"checkout.line_items must not be empty; cancel the " +
				"checkout to abandon it"))
	}
	// Every lookup and refusal before the first cart write: a refused
	// update must leave the cart and the stored session agreeing.
	// Nothing here is saved until the end.
	in.Checkout.applyTo(s)
	if err := h.applyRegion(ctx, t, s, &in.Checkout); err != nil {
		return refusedBy(err)
	}
	if err := h.applyPayment(ctx, t, s, &in.Checkout, neg); err != nil {
		return refusedBy(err)
	}
	codes, hasCodes := in.Checkout.discountCodes()
	if in.Checkout.LineItems != nil || hasCodes {
		// The lookups above can outlast the lock. A checkout another call
		// stored meanwhile, a completion included, gets none of this
		// update's cart writes; finish answers by what is stored.
		if err := h.deps.Checkout.Current(ctx, s); err != nil {
			return refusedBy(saveFailed(err))
		}
	}
	if in.Checkout.LineItems != nil {
		if err := checkout.SyncCartLines(
			ctx, h.deps.Django, t, s.Protocol, s.CartID, lines,
		); err != nil {
			return refusedBy(withHint(upstreamErr(err, cartGone),
				checkoutNotUpdated))
		}
	}
	if hasCodes {
		if err := checkout.ApplyDiscountCodes(
			ctx, h.deps.Django, t, s, codes,
		); err != nil {
			return refusedBy(withHint(upstreamErr(err, cartGone),
				checkoutNotUpdated))
		}
	}
	s.Recompute()
	// The lookups above can outlast the lock, and another call may have
	// stored the checkout meanwhile, a completion included: this
	// snapshot is then not written over it. The cart writes above are
	// already made.
	if err := h.deps.Checkout.Save(ctx, s); err != nil {
		return refusedBy(withHint(saveFailed(err), checkoutNotUpdated))
	}
	return afterSave(func() (*mcp.CallToolResult, CheckoutResult, error) {
		res, out, err := h.checkoutResult(ctx, t, s, neg)
		return h.discountAnnotated(res, out, err, s, hasCodes)
	})
}

func (h *handlers) completeCheckout(
	ctx context.Context, _ *mcp.CallToolRequest, in CompleteCheckoutIn,
) (*mcp.CallToolResult, CheckoutResult, error) {
	var zero CheckoutResult
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	if err := in.Meta.validate(true); err != nil {
		return nil, zero, err
	}
	neg, err := h.negotiate(ctx, t, in.Meta, h.linksFor(ctx, t).Cart())
	if err != nil {
		return nil, zero, err
	}
	if !neg.Has(ucp.CapabilityCheckout) {
		return nil, incompatibleCheckout(h.linksFor(ctx, t)), nil
	}
	s, release, err := h.lockedSession(ctx, t, in.ID)
	if err != nil {
		return nil, zero, err
	}
	defer release()
	return h.finish(ctx, t, s, h.completeSession(ctx, t, s, in, neg),
		func(now *checkout.Session) toolResult {
			return h.completeClosed(ctx, t, now, neg)
		})
}

// completeClosed answers a complete of a checkout that is not open, and
// is nil for one that is. One that placed its order is answered with
// the checkout: completed, or awaiting the payment its continue_url
// leads to. The order is what a complete asks for, and it exists.
func (h *handlers) completeClosed(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	neg ucp.Negotiated,
) toolResult {
	switch s.Status {
	case checkout.StatusCompleted, checkout.StatusRequiresEscalation:
		return func() (*mcp.CallToolResult, CheckoutResult, error) {
			return h.checkoutResult(ctx, t, s, neg)
		}
	case checkout.StatusCompleteInProgress, checkout.StatusCanceled:
		return refusedBy(h.closedTo(ctx, s, "completed")).answer
	}
	return nil
}

// completeSession is complete_checkout once its checkout is loaded.
func (h *handlers) completeSession(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	in CompleteCheckoutIn, neg ucp.Negotiated,
) sessionAnswer {
	render := func() (*mcp.CallToolResult, CheckoutResult, error) {
		return h.checkoutResult(ctx, t, s, neg)
	}
	// The escalation resolves through the order event, never through a
	// second complete, except to retry a payment link that could not be
	// created, for an order that already exists.
	if s.Status == checkout.StatusRequiresEscalation && s.PaymentURL == "" {
		if err := h.deps.Flow.ResumePayment(ctx, t, s); err != nil {
			return refusedBy(paymentLinkErr(s))
		}
		if err := h.deps.Checkout.Save(ctx, s); err != nil {
			return refusedBy(saveFailed(err))
		}
		return afterSave(render)
	}
	if closed := h.completeClosed(ctx, t, s, neg); closed != nil {
		return fromSnapshot(closed)
	}

	// The submitted instrument decides how this order settles. Resolving
	// it here — rather than trusting a pay-way set earlier — is what
	// makes the payment object authoritative, and it rejects an
	// instrument the store cannot honour instead of placing an order the
	// buyer has no way to pay for.
	payWay, status := s.PayWayID, s.Status
	if err := h.applyPayment(ctx, t, s, &in.Checkout, neg); err != nil {
		return refusedBy(err)
	}
	if s.PayWayID <= 0 {
		return refusedBy(errors.New(
			"checkout.payment.instruments is required to complete: " +
				"submit one of the instruments the checkout advertised"))
	}
	s.Recompute()

	claim, err := h.deps.Checkout.ClaimCompletion(
		ctx, t.SchemaName, s.ID, in.Meta.IdempotencyKey)
	if err != nil {
		if errors.Is(err, checkout.ErrCompletionInProgress) {
			return refusedBy(err)
		}
		return refusedBy(errCheckoutUnavailable)
	}
	if claim == nil {
		// A prior attempt with this key finished. What it made of the
		// checkout is what is stored now, which finish answers with:
		// this call's snapshot may predate it.
		return fromSnapshot(render)
	}

	// What this call submitted is the buyer's latest choice whether or
	// not the call places the order. It is stored now, because a refused
	// completion stores nothing: a later complete that omits the payment
	// would otherwise place the order with the choice before this one.
	if s.Open() && (s.PayWayID != payWay || s.Status != status) {
		if err := h.deps.Checkout.Save(ctx, s); err != nil {
			h.settleCompletion(ctx, s, claim, checkout.NothingPlaced)
			return refusedBy(saveFailed(err))
		}
	}

	placement, err := h.deps.Flow.Complete(ctx, t, s)
	h.settleCompletion(ctx, s, claim, placement)
	if placement == checkout.NothingPlaced {
		// Refused before the order was sent, or by Django for the order
		// itself: either way nothing of this call exists.
		return refusedBy(completeErr(s, err))
	}
	// Past the in-progress save, which only the stored checkout's own
	// call passes: what follows is this call's own outcome.
	return afterSave(func() (*mcp.CallToolResult, CheckoutResult, error) {
		if err != nil {
			return nil, CheckoutResult{}, completeErr(s, err)
		}
		if ctx.Err() != nil {
			// The call's context has ended: the caller hung up, or the
			// server is shutting down. Reading the order back on it
			// would only fail, so the call is answered as for any
			// placed checkout that could not be rendered.
			h.deps.Log.InfoContext(ctx,
				"ucp: order placed, checkout not rendered",
				slog.String("session", s.ID),
				slog.String("order", s.OrderUUID),
				slog.String("cause", context.Cause(ctx).Error()))
			return nil, CheckoutResult{}, unrenderedErr(s)
		}
		return render()
	})
}

// completeErr is the tool's answer for an error of Flow.Complete.
func completeErr(s *checkout.Session, err error) error {
	switch {
	case errors.Is(err, checkout.ErrOrderOutcomeUnknown):
		return errOutcomeUnknown
	case errors.Is(err, checkout.ErrPaymentSessionFailed):
		return paymentLinkErr(s)
	case errors.Is(err, checkout.ErrSessionNotSaved):
		return placedUnsavedErr(s.OrderUUID)
	case errors.Is(err, checkout.ErrUnavailable):
		return errCheckoutUnavailable
	case errors.As(err, new(*django.StockShortfall)):
		return stockShortfallErr(err)
	case errors.Is(err, checkout.ErrSessionChanged):
		return saveFailed(err)
	case errors.Is(err, checkout.ErrNotReady),
		errors.Is(err, checkout.ErrCompletionInProgress):
		return err
	case errors.Is(err, checkout.ErrPayWayUnavailable):
		return errors.New(
			"the selected payment method is not offered for this " +
				"delivery; submit another advertised instrument")
	case errors.Is(err, checkout.ErrPaymentMethodUnsupported):
		return errors.New(
			"this payment method needs the buyer to pay on the " +
				"store's checkout; use an offline method (e.g. cash " +
				"on delivery), Viva card payment, or hand over " +
				"get_checkout_link instead")
	}
	return upstreamErr(err, "order placement failed; the cart is unchanged")
}

// placedUnsavedErr answers for an order that is placed while its
// checkout never recorded it: to the completion whose last save failed,
// and to every later call, which finds the checkout in progress and the
// order named beside it.
func placedUnsavedErr(orderUUID string) error {
	return errors.New(
		"the order was placed but checkout state could not be saved; " +
			"track it with track_order using orderUuid " + orderUUID)
}

// settleCompletion spends or releases a completion claim by what the
// attempt did about the order. It runs even when the call's context has
// ended (a platform that hung up, a shutdown): a retry must find what
// this attempt did, not a claim left in flight. The Django client's
// timeout bounds the one Redis write, which go-redis alone would retry
// against a silent Redis for several times that.
//
// A claim that lapsed while the attempt ran may be another call's by
// now, and is left to it. The answer does not depend on it: what this
// attempt did about the order stands, and the session says so to
// whoever holds the key.
func (h *handlers) settleCompletion(
	ctx context.Context, s *checkout.Session, claim *checkout.Claim,
	placement checkout.Placement,
) {
	settleCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), h.deps.Django.Timeout())
	defer cancel()
	var err error
	if placement == checkout.NothingPlaced {
		err = claim.Release(settleCtx)
	} else {
		var spent bool
		spent, err = h.deps.Checkout.MarkCompleted(settleCtx, claim)
		if err == nil && !spent {
			err = errors.New(
				"the claim lapsed and another call has the key")
		}
	}
	if err != nil {
		h.deps.Log.ErrorContext(ctx, "ucp: completion key not settled",
			slog.String("session", s.ID),
			slog.Bool("spent", placement != checkout.NothingPlaced),
			slog.String("error", err.Error()))
	}
}

// errCheckoutChanged answers a checkout another call stored while this
// one ran: nothing of this call was stored in it.
var errCheckoutChanged = &mcptool.ToolError{Code: mcptool.CodeConflict,
	Hint: "this checkout was changed by another request while this one " +
		"ran, so nothing of this call was stored in it; read it with " +
		"get_checkout before trying again"}

// saveFailed answers a checkout that could not be stored: one another
// call stored meanwhile is to be read again, anything else is the store
// being unavailable.
func saveFailed(err error) error {
	if errors.Is(err, checkout.ErrSessionChanged) {
		return errCheckoutChanged
	}
	return errCheckoutUnavailable
}

// closedTo answers an update, a cancel or a complete (action: "updated",
// "canceled", "completed") of a checkout that takes it no more, and is
// nil for one that is open: an order that is being placed, a canceled
// checkout, and one that placed an order, which canceling or changing
// the checkout would not touch.
func (h *handlers) closedTo(
	ctx context.Context, s *checkout.Session, action string,
) error {
	switch {
	case s.Open():
		return nil
	case s.Status == checkout.StatusCompleteInProgress:
		return h.pending(ctx, s)
	case s.Status == checkout.StatusCanceled:
		return fmt.Errorf("checkout %s is canceled and cannot be %s",
			s.ID, action)
	}
	return fmt.Errorf(
		"checkout %s already placed order %s and cannot be %s here; "+
			"the order is managed by the store, and get_order or "+
			"track_order reads it", s.ID, s.OrderUUID, action)
}

// errOutcomeUnknown answers a checkout whose order the store never
// confirmed or denied, once its completion can no longer be running: the
// session stays so until it expires, and no call changes that. It tells
// the agent what a person can do, because nothing the agent can call
// will find out.
var errOutcomeUnknown = errors.New(
	"the store never confirmed whether this checkout's order was " +
		"placed, and that will not change: do not complete it again, " +
		"do not keep checking it, and do not start another checkout. " +
		"Ask the shopper to look for the store's order confirmation " +
		"email, or to ask the store; start a new checkout only if " +
		"they find that no order was placed")

// pending answers a call that finds the checkout's order being placed.
// An order the completion placed and could not save is known (the
// checkout or the store names it): that is the answer. Else, while the
// completion can still run, to check again shortly; past that
// (Flow.CompletionDeadline, as ACP judges it), that its outcome was
// never recorded.
func (h *handlers) pending(ctx context.Context, s *checkout.Session) error {
	if order := h.deps.Flow.PlacedOrder(ctx, s); order != "" {
		return placedUnsavedErr(order)
	}
	if time.Until(h.deps.Flow.CompletionDeadline(s)) > 0 {
		return checkout.ErrCompletionInProgress
	}
	return errOutcomeUnknown
}

// errCheckoutGone answers a checkout id no session is stored for. The id
// does not say why: the checkout was never completed and expired, it
// placed an order and its session is no longer kept, or the id is wrong.
// So the answer never says only to start another: an agent that did
// would order a second time in the second case.
var errCheckoutGone = errors.New(
	"that checkout no longer exists here. It may never have been " +
		"completed, it may have placed an order whose checkout is no " +
		"longer kept, or the id may be wrong, so an order may exist: " +
		"do not start another checkout before checking. If you hold " +
		"its order id, read the order with get_order or track_order; " +
		"if not, ask the shopper for the store's order confirmation. " +
		"Only if it was never completed, start a new one with " +
		"create_checkout")

// errCheckoutUnavailable answers a checkout store (Redis) that could not
// be read or written: the sessions, their locks and completion claims,
// and the order links.
var errCheckoutUnavailable = &mcptool.ToolError{Code: mcptool.CodeUnavailable,
	RetryAfter: mcptool.UnavailableRetryAfter,
	Hint: "the store's checkout records are temporarily unavailable; " +
		"retry shortly, then tell the shopper to try again later"}

// What a create or update whose cart write failed left behind, added to
// the hint for its cause (withHint). An update's cart writes cannot be
// undone: lines may already be in the cart while the session keeps its
// old buyer, fulfillment and payment.
const (
	checkoutNotCreated = "No checkout was created"
	checkoutNotUpdated = "The checkout was not updated: its buyer, " +
		"fulfillment and payment were not stored, though line items it " +
		"sent may already be in the cart"
	// cartGone is a checkout's cart Django no longer serves for its id:
	// each retry would reach a new guest cart, never this one.
	cartGone = "the checkout's cart no longer exists; start a new one " +
		"with create_checkout"
)

// unrenderedErr is the answer for an order that exists while its
// checkout is not rendered: the agent must read it again, not order
// again.
func unrenderedErr(s *checkout.Session) error {
	return fmt.Errorf(
		"order %s is placed, but the checkout could not be rendered; "+
			"read it again with get_checkout, and do not start a "+
			"new checkout", s.OrderUUID)
}

// paymentLinkErr is the truthful answer when the order exists but its
// hosted payment link could not be created: saying the order failed
// would send the buyer to check out again and order twice.
func paymentLinkErr(s *checkout.Session) error {
	return fmt.Errorf(
		"order %s is placed, but its payment link could not be created; "+
			"call complete_checkout again for checkout %s to retry the "+
			"payment link — do not start a new checkout",
		s.OrderUUID, s.ID)
}

func stockShortfallErr(err error) error {
	shortfall, _ := errors.AsType[*django.StockShortfall](err)
	lines := make([]string, 0, len(shortfall.FailedItems))
	for _, item := range shortfall.FailedItems {
		lines = append(lines, fmt.Sprintf(
			"%s (product %d): requested %d, only %d available",
			item.ProductName, item.ProductID,
			item.Requested, item.Available))
	}
	return fmt.Errorf(
		"not enough stock: %s — adjust quantities and retry",
		strings.Join(lines, "; "))
}

func (h *handlers) lockedSession(
	ctx context.Context, t *tenant.Tenant, id string,
) (*checkout.Session, func(), error) {
	if id == "" {
		return nil, nil, errors.New("id is required")
	}
	release, err := h.deps.Checkout.Lock(ctx, t.SchemaName, id)
	if err != nil {
		switch {
		case errors.Is(err, checkout.ErrLocked):
			return nil, nil, err
		case errors.Is(err, checkout.ErrNotFound):
			return nil, nil, errCheckoutGone
		}
		return nil, nil, errCheckoutUnavailable
	}
	s, err := h.deps.Checkout.Load(ctx, t.SchemaName, id)
	if err != nil {
		release()
		if errors.Is(err, checkout.ErrNotFound) {
			return nil, nil, errCheckoutGone
		}
		return nil, nil, errCheckoutUnavailable
	}
	return s, release, nil
}

func (h *handlers) checkoutResult(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	neg ucp.Negotiated,
) (*mcp.CallToolResult, CheckoutResult, error) {
	var zero CheckoutResult
	payload, err := h.deps.UCP.BuildCheckout(ctx, t, s,
		neg.Declaration(ucp.CapabilityCheckout))
	if err != nil {
		switch {
		case s.OrderUUID != "":
			return nil, zero, unrenderedErr(s)
		case s.MayHaveOrder():
			// Being placed, or never confirmed: its cart may have gone
			// with the order, so it is answered as what it is, not as a
			// checkout to read again until it renders.
			return nil, zero, h.pending(ctx, s)
		}
		return nil, zero, upstreamErr(err,
			"checkout state could not be rendered; retry")
	}

	var text string
	switch s.Status {
	case checkout.StatusIncomplete:
		text = fmt.Sprintf(
			"Checkout %s is incomplete — still needed: %s.",
			s.ID, strings.Join(s.Missing(), "; "))
	case checkout.StatusReadyForComplete:
		text = fmt.Sprintf(
			"Checkout %s is ready — call complete_checkout to place the "+
				"order.", s.ID)
	case checkout.StatusRequiresEscalation:
		text = fmt.Sprintf(
			"Order placed. The buyer must now authorize payment at %s "+
				"(the store's hosted card checkout). The session "+
				"completes automatically once payment lands.",
			payload.ContinueURL)
	case checkout.StatusCompleted:
		text = fmt.Sprintf(
			"Order %s is placed and confirmed. Track it with track_order.",
			s.OrderUUID)
	case checkout.StatusCompleteInProgress:
		// The guidance every other tool gives for this state: in flight,
		// the order it placed, or an outcome that was never recorded.
		text = h.pending(ctx, s).Error()
	default:
		text = fmt.Sprintf("Checkout %s status: %s.", s.ID, s.Status)
	}
	return textResult("%s", text), CheckoutResult{checkout: payload}, nil
}

// discountAnnotated appends the outcome of a discount submission to the
// tool's summary text so the agent sees rejections without digging into
// the session state.
func (h *handlers) discountAnnotated(
	res *mcp.CallToolResult, out CheckoutResult, err error,
	s *checkout.Session, submitted bool,
) (*mcp.CallToolResult, CheckoutResult, error) {
	if err != nil || !submitted || len(s.RejectedDiscounts) == 0 {
		return res, out, err
	}
	var notes []string
	for _, rej := range s.RejectedDiscounts {
		note := fmt.Sprintf("discount code %q was rejected (%s)",
			rej.Code, rej.Reason)
		if rej.Message != "" {
			note += ": " + rej.Message
		}
		notes = append(notes, note)
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		tc.Text += " Note: " + strings.Join(notes, "; ") + "."
	}
	return res, out, err
}

// getCheckout re-renders a session. Read-only, so it takes no lock and
// no idempotency key.
func (h *handlers) getCheckout(
	ctx context.Context, _ *mcp.CallToolRequest, in GetCheckoutIn,
) (*mcp.CallToolResult, CheckoutResult, error) {
	var zero CheckoutResult
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	if err := in.Meta.validate(false); err != nil {
		return nil, zero, err
	}
	neg, err := h.negotiate(ctx, t, in.Meta, h.linksFor(ctx, t).Cart())
	if err != nil {
		return nil, zero, err
	}
	if !neg.Has(ucp.CapabilityCheckout) {
		return nil, incompatibleCheckout(h.linksFor(ctx, t)), nil
	}
	if in.ID == "" {
		return nil, zero, errors.New("id is required")
	}
	s, err := h.deps.Checkout.Load(ctx, t.SchemaName, in.ID)
	if err != nil {
		if errors.Is(err, checkout.ErrNotFound) {
			return nil, zero, errCheckoutGone
		}
		return nil, zero, errCheckoutUnavailable
	}
	return h.checkoutResult(ctx, t, s, neg)
}

// cancelCheckout abandons a session the buyer is no longer pursuing.
//
// A terminal session is re-rendered rather than refused: cancel is
// idempotent by nature, and a platform retrying after a lost response
// must not receive an error for work already done.
func (h *handlers) cancelCheckout(
	ctx context.Context, _ *mcp.CallToolRequest, in CancelCheckoutIn,
) (*mcp.CallToolResult, CheckoutResult, error) {
	var zero CheckoutResult
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	if err := in.Meta.validate(true); err != nil {
		return nil, zero, err
	}
	neg, err := h.negotiate(ctx, t, in.Meta, h.linksFor(ctx, t).Cart())
	if err != nil {
		return nil, zero, err
	}
	if !neg.Has(ucp.CapabilityCheckout) {
		return nil, incompatibleCheckout(h.linksFor(ctx, t)), nil
	}
	s, release, err := h.lockedSession(ctx, t, in.ID)
	if err != nil {
		return nil, zero, err
	}
	defer release()
	return h.finish(ctx, t, s, h.cancelSession(ctx, t, s, neg),
		func(now *checkout.Session) toolResult {
			return h.cancelClosed(ctx, t, now, neg)
		})
}

// cancelClosed answers a cancel of a checkout that is not open, and is
// nil for one that is. A canceled one is rendered: a repeat cancel is
// not an error. Once an order exists, or may (being placed, awaiting
// payment, completed), canceling the checkout would not cancel the
// order: the buyer could still pay it, and the session would read
// canceled over a paid order.
func (h *handlers) cancelClosed(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	neg ucp.Negotiated,
) toolResult {
	if s.Status == checkout.StatusCanceled {
		return func() (*mcp.CallToolResult, CheckoutResult, error) {
			return h.checkoutResult(ctx, t, s, neg)
		}
	}
	if err := h.closedTo(ctx, s, "canceled"); err != nil {
		return refusedBy(err).answer
	}
	return nil
}

// cancelSession is cancel_checkout once its checkout is loaded.
func (h *handlers) cancelSession(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	neg ucp.Negotiated,
) sessionAnswer {
	if closed := h.cancelClosed(ctx, t, s, neg); closed != nil {
		return fromSnapshot(closed)
	}
	if err := h.cancel(ctx, s); err != nil {
		return refusedBy(saveFailed(err))
	}
	return afterSave(func() (*mcp.CallToolResult, CheckoutResult, error) {
		return h.checkoutResult(ctx, t, s, neg)
	})
}

// incompatibleCheckout is the negotiation-failure result: the platform
// shares no version of the checkout capability.
func incompatibleCheckout(links storefront.Links) CheckoutResult {
	return CheckoutResult{
		failure: ucp.CapabilitiesIncompatible(links.Cart()),
	}
}
