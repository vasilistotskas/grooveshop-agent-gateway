package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcptool"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// The UCP checkout tools return the spec's checkout session object as
// structuredContent (the MCP transport binding requirement). Their input
// schemas are the spec's (ucp_inputs.go), read from tools/list.

// CreateCheckoutIn is the create_checkout tool's arguments.
type CreateCheckoutIn struct {
	Meta     *MetaIn       `json:"meta"`
	Checkout UCPCheckoutIn `json:"checkout"`
	// CartID starts the checkout from a cart built with the cart tools.
	// UCP models that in its cart capability (dev.ucp.shopping.cart),
	// as a cart_id member of the checkout object. This business does
	// not advertise that capability, so the member is the gateway's own:
	// the create schema names it beside `checkout`, outside the
	// negotiated shape, and a caller sends it or checkout.line_items. The
	// capability's aim is kept, a cart has one incomplete checkout, but
	// not its means: a checkout created unsigned is bound to no platform,
	// so the spec's "return the existing checkout" is not offered. The newest
	// create holds the cart, and an open checkout on it is canceled
	// (createFromCart). (Order webhooks are discovered from the platform
	// profile, never taken from a request.)
	CartID string `json:"cart_id"`
}

// GetCheckoutIn is the get_checkout tool's arguments.
type GetCheckoutIn struct {
	Meta *MetaIn `json:"meta"`
	ID   string  `json:"id"`
}

// UpdateCheckoutIn is the update_checkout tool's arguments.
type UpdateCheckoutIn struct {
	Meta     *MetaIn       `json:"meta"`
	ID       string        `json:"id"`
	Checkout UCPCheckoutIn `json:"checkout"`
}

// CompleteCheckoutIn is the complete_checkout tool's arguments.
type CompleteCheckoutIn struct {
	Meta *MetaIn `json:"meta"`
	ID   string  `json:"id"`
	// Checkout carries the payment object the business needs to settle.
	Checkout UCPPaymentChoice `json:"checkout"`
}

// CancelCheckoutIn is the cancel_checkout tool's arguments.
type CancelCheckoutIn struct {
	Meta *MetaIn `json:"meta"`
	ID   string  `json:"id"`
}

// checkoutBody is a canonical checkout tool once its call has passed
// the steps every one of them opens with (negotiated).
type checkoutBody func(
	t *tenant.Tenant, neg ucp.Negotiated,
) (*mcp.CallToolResult, CheckoutResult, error)

// negotiated runs what every canonical checkout tool opens with, then
// body: the store the request is for, the call's meta (keyed: the
// operation needs an idempotency key), and the platform's profile
// negotiated. A platform that shares no version of the checkout
// capability is answered with the spec's error response, and body does
// not run.
func (h *handlers) negotiated(
	ctx context.Context, m *MetaIn, keyed bool, body checkoutBody,
) (*mcp.CallToolResult, CheckoutResult, error) {
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, CheckoutResult{}, err
	}
	if err := m.validate(keyed); err != nil {
		return nil, CheckoutResult{}, err
	}
	links := h.linksFor(ctx, t)
	neg, err := h.negotiate(ctx, t, m, links.Cart())
	if err != nil {
		return nil, CheckoutResult{}, err
	}
	if !neg.Has(ucp.CapabilityCheckout) {
		return nil, incompatibleCheckout(links), nil
	}
	res, out, err := body(t, neg)
	if errors.Is(err, errCheckoutGone) {
		// No checkout is kept for the id: the spec's error response,
		// whichever step found it gone.
		return nil, CheckoutResult{failure: ucp.CheckoutNotFound(
			neg.Declaration(ucp.CapabilityCheckout), errCheckoutGone.Hint),
		}, nil
	}
	return res, out, err
}

// onSession runs body on the checkout id names, under its lock, and
// gives body's answer through finish: stored is the operation's answer
// for a checkout another request stored meanwhile.
func (h *handlers) onSession(
	ctx context.Context, t *tenant.Tenant, id string,
	body func(s *checkout.Session) sessionAnswer,
	stored func(now *checkout.Session) toolResult,
) (*mcp.CallToolResult, CheckoutResult, error) {
	s, release, err := h.lockedSession(ctx, t, id)
	if err != nil {
		return nil, CheckoutResult{}, err
	}
	defer release()
	return h.finish(ctx, t, s, body(s), stored)
}

func (h *handlers) createCheckout(
	ctx context.Context, _ *mcp.CallToolRequest, in CreateCheckoutIn,
) (*mcp.CallToolResult, CheckoutResult, error) {
	return h.negotiated(ctx, in.Meta, false,
		func(t *tenant.Tenant, neg ucp.Negotiated) (
			*mcp.CallToolResult, CheckoutResult, error,
		) {
			return h.create(ctx, t, in, neg)
		})
}

// create is create_checkout once its call is negotiated.
func (h *handlers) create(
	ctx context.Context, t *tenant.Tenant, in CreateCheckoutIn,
	neg ucp.Negotiated,
) (*mcp.CallToolResult, CheckoutResult, error) {
	var zero CheckoutResult
	lines, err := in.Checkout.lines()
	if err != nil {
		return nil, zero, err
	}
	switch {
	case in.CartID != "" && len(lines) > 0:
		return nil, zero, invalidInput(
			"send either cart_id or checkout.line_items, not both: the " +
				"cart already holds its lines")
	case in.CartID == "" && len(lines) == 0:
		return nil, zero, invalidInput(
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
	hasCodes, err := h.applyCodes(ctx, t, s, &in.Checkout, neg,
		"the cart was not found; start a new checkout with line_items")
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
		} else {
			h.discard(ctx, s)
		}
		return nil, zero, errCheckoutUnavailable
	}
	return h.created(ctx, t, s, neg, hasCodes, cartGone)
}

// created answers a create with the checkout it just stored. One that
// cannot be rendered was given to nobody, so it is discarded: a create
// that fails leaves no checkout. gone is the hint for a cart Django no
// longer serves, which the render is the first to find.
func (h *handlers) created(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	neg ucp.Negotiated, hasCodes bool, gone string,
) (*mcp.CallToolResult, CheckoutResult, error) {
	res, out, err := h.checkoutResult(ctx, t, s, neg)
	if err != nil {
		h.discard(ctx, s)
		if errors.Is(err, errCheckoutCartGone) {
			err = &mcptool.ToolError{Code: mcptool.CodeNotFound, Hint: gone}
		}
		return nil, CheckoutResult{}, withHint(err, checkoutNotCreated)
	}
	return h.discountAnnotated(res, out, nil, s, hasCodes)
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
	// The platform that signed the create owns the checkout, and the
	// order it places; an unsigned create leaves it unowned.
	s.Platform, _ = signerFrom(ctx)
	delivered, err := in.applyTo(s, neg)
	if err != nil {
		return nil, err
	}
	if err := h.applyRegion(ctx, t, s, delivered); err != nil {
		return nil, err
	}
	if err := h.applyPayment(ctx, t, s, &in.UCPPaymentChoice, neg); err != nil {
		return nil, err
	}
	return s, nil
}

// applyCodes writes a create's discount codes to its cart, and reports
// whether it sent any. gone is the hint for a cart Django no longer
// serves, which the coupon call can be the first to find.
func (h *handlers) applyCodes(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	in *UCPCheckoutIn, neg ucp.Negotiated, gone string,
) (bool, error) {
	codes, hasCodes := in.discountCodes(neg)
	if !hasCodes {
		return false, nil
	}
	if err := checkout.ApplyDiscountCodes(
		ctx, h.deps.Django, t, s, codes,
	); err != nil {
		return true, withHint(upstreamErr(err, gone), checkoutNotCreated)
	}
	return true, nil
}

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
// and any cart write. The create's region and payment are settled
// before the holder is canceled: a create the store refuses there
// cancels nothing.
//
// The cart is written only by the checkout that holds it, in that
// checkout's turn. So the create's discount codes go to the cart after
// the new checkout has taken it, under the new checkout's lock, which
// is held from before the checkout is stored (takeCart): a later create
// that finds it on the cart waits for that write (supersede). A create
// that fails after it took the cart (the code write, the save after it,
// the render) leaves no checkout: the new one is discarded, and the one
// it replaced stays canceled.
func (h *handlers) createFromCart(
	ctx context.Context, t *tenant.Tenant, in CreateCheckoutIn,
	neg ucp.Negotiated,
) (*mcp.CallToolResult, CheckoutResult, error) {
	var zero CheckoutResult
	// Django reads every spelling of a UUID as one cart, so the cart is
	// named in one spelling here: it keys the cart's record.
	cartID, ok := checkout.CanonicalCartID(in.CartID)
	if !ok {
		return nil, zero, invalidInput(
			"cart_id is not a cart id: send the cartId the cart tools " +
				"returned, or omit it and send checkout.line_items")
	}
	// A holder that changed while it was read is decided again by
	// takeCart.
	if _, err := h.cartHolder(ctx, t, cartID); err != nil &&
		!errors.Is(err, checkout.ErrSessionChanged) {
		return nil, zero, err
	}
	s, err := h.newSession(ctx, t, &in.Checkout, neg)
	if err != nil {
		return nil, zero, err
	}
	s.CartID = cartID
	s.Recompute()

	release, err := h.takeCart(ctx, t, s)
	if err != nil {
		return nil, zero, err
	}
	defer release()

	hasCodes, err := h.applyCodes(ctx, t, s, &in.Checkout, neg, cartIDGone)
	if err != nil {
		h.discard(ctx, s)
		return nil, zero, err
	}
	if hasCodes {
		s.Recompute()
		if err := h.deps.Checkout.Save(ctx, s); err != nil {
			// Stored since by another create that took the cart, or
			// the store failed: either way nobody was given s.
			h.discard(ctx, s)
			answer := error(errCheckoutUnavailable)
			if errors.Is(err, checkout.ErrSessionChanged) {
				answer = errCartBusy
			}
			return nil, zero, withHint(answer,
				"The discount codes this call sent may already be "+
					"on the cart")
		}
	}
	// A cart Django no longer serves (a guest cart an order emptied or a
	// sign-in merged is removed) is cartIDGone: an order may exist.
	return h.created(ctx, t, s, neg, hasCodes, cartIDGone)
}

// cartCreateRounds bounds how often a create naming a cart decides
// again about the cart's holder, after another request changed it or
// was at work on it.
const cartCreateRounds = 3

// takeCart stores s as the checkout that holds its cart, canceling the
// checkout that held it where the create may (cartHolder); a holder
// that keeps the cart is the create's refusal. Each round reads the
// holder again: the create's lookups took time, and every further round
// follows another request's change to the holder.
//
// It returns with s's lock held, for the cart writes that follow, and
// release frees it. The lock is taken before s is first stored, and no
// earlier, so that the lookups a round makes are not counted against
// it.
func (h *handlers) takeCart(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
) (release func(), err error) {
	defer func() {
		if err != nil && release != nil {
			release()
			release = nil
		}
	}()
	for range cartCreateRounds {
		holder, err := h.cartHolder(ctx, t, s.CartID)
		if errors.Is(err, checkout.ErrSessionChanged) {
			// Another create took the cart while it was read.
			continue
		}
		if err != nil {
			return release, err
		}
		if !holder.Free() {
			// An open checkout, or one whose order Django canceled.
			err = h.supersede(ctx, holder.Session)
			if errors.Is(err, checkout.ErrLocked) ||
				errors.Is(err, checkout.ErrSessionChanged) {
				// A call is at work on the holder, or one just stored
				// it: perhaps it is placing its order by now.
				continue
			}
			if err != nil {
				return release, errCheckoutUnavailable
			}
		}
		if release == nil {
			release, err = h.deps.Checkout.Lock(ctx, s.Schema, s.ID)
			if err != nil {
				return nil, errCheckoutUnavailable
			}
		}
		err = h.deps.Checkout.Create(ctx, s, holder)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, checkout.ErrCartHeld) {
			h.discard(ctx, s)
			return release, errCheckoutUnavailable
		}
		// Another create took the cart since it was read.
	}
	return release, errCartBusy
}

// supersede cancels the checkout whose cart a create takes (an open
// one, or one whose unpaid order Django canceled), in that checkout's
// own turn: under its lock, as cancel_checkout does. A
// call at work on it (an update writing the cart, a completion) holds
// the lock, and the create does not cancel under it: Django's cart is
// outside the session's compare-and-set, and only the lock keeps an
// update's cart writes out of the next checkout's cart.
func (h *handlers) supersede(
	ctx context.Context, old *checkout.Session,
) error {
	release, err := h.deps.Checkout.Lock(ctx, old.Schema, old.ID)
	if err != nil {
		return err
	}
	defer release()
	if err := h.cancel(ctx, old); err != nil {
		return err
	}
	h.deps.Log.InfoContext(ctx,
		"ucp: a create canceled the checkout that held its cart",
		slog.String("session", old.ID))
	return nil
}

// discard removes a checkout its create stored and never answered, on
// a context the caller cannot end: the caller hanging up is one way a
// create fails after its checkout was stored. It also follows a store
// that failed the create itself: whether the checkout was stored is not
// known then (the script may have run and its reply been lost), and
// discarding one that was not stored removes nothing.
func (h *handlers) discard(ctx context.Context, s *checkout.Session) {
	ctx, cancel := h.deps.Checkout.Settling(ctx)
	defer cancel()
	if err := h.deps.Checkout.Discard(ctx, s); err != nil {
		h.deps.Log.ErrorContext(ctx,
			"ucp: unanswered checkout not discarded",
			slog.String("session", s.ID),
			slog.String("error", err.Error()))
	}
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
// (checkout.CartHolder.Free), or is one the create cancels: an open UCP
// checkout, or a checkout whose order awaited its payment and was
// canceled by Django (orderCanceled). A holder that keeps the cart is
// the create's refusal:
//   - a checkout whose order is being placed or awaits its payment: an
//     order exists, or may, and a second checkout could place a second
//     one;
//   - an open checkout this caller may not reach (Session.Serves): one
//     of another protocol, which is that surface's to cancel, or one
//     another platform signed the creation of, which is that platform's.
//     One whose order Django canceled is canceled here whatever its
//     protocol or platform: nothing is left of it to finish, and its own
//     surface refuses to cancel a checkout that placed an order;
//   - a checkout this release does not read (CartHolder.Unread): one of
//     another release while a key move rolls out, or one Redis lost. An
//     order may exist, and nothing of it can be canceled here.
//
// No refusal names the holder or its order. A holder that changed while
// it was read (another create took the cart) is
// checkout.ErrSessionChanged, which takeCart decides again on.
func (h *handlers) cartHolder(
	ctx context.Context, t *tenant.Tenant, cartID string,
) (checkout.CartHolder, error) {
	holder, err := h.deps.Checkout.HolderOf(ctx, t.SchemaName, cartID)
	if errors.Is(err, checkout.ErrSessionChanged) {
		return holder, err
	}
	if err != nil {
		return holder, errCheckoutUnavailable
	}
	s := holder.Session
	switch {
	case holder.Free():
	case holder.Unread():
		return holder, errCartUnread
	case s.MayHaveOrder():
		canceled, err := h.orderCanceled(ctx, t, s)
		if err != nil {
			return holder, err
		}
		if !canceled {
			return holder, cartOrdered(s)
		}
	case !s.Serves(ucpCaller(ctx)):
		return holder, errCartElsewhere
	}
	return holder, nil
}

// orderCanceled reports whether Django canceled the order a checkout
// placed and still reads as awaiting its payment. Django cancels an
// unpaid online order when its payment window ends, and keeps the cart;
// the session reads "awaiting payment" for its whole day, because only
// a payment changes it. So the order is asked, not the session: the
// cart of a canceled order is free, and the shopper who left the
// payment page can check out again. An order Django no longer has, or
// one in any other state, keeps the cart.
func (h *handlers) orderCanceled(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
) (bool, error) {
	if s.Status != checkout.StatusRequiresEscalation || s.OrderUUID == "" {
		return false, nil
	}
	o, err := h.deps.Django.PlacedOrder(
		ctx, t.Domain, t.DefaultLocale, s.OrderUUID)
	switch {
	case errors.Is(err, django.ErrNotFound):
		return false, nil
	case err != nil:
		return false, withHint(upstreamErr(err,
			"whether this cart's earlier order still awaits its "+
				"payment could not be read; retry shortly"),
			checkoutNotCreated)
	}
	return o.Status == django.OrderStatusCanceled, nil
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

	// errCartElsewhere: an open checkout this caller may not reach holds
	// the cart.
	errCartElsewhere = &mcptool.ToolError{Code: mcptool.CodeConflict,
		Hint: "this cart is already in an open checkout that was " +
			"started outside this server or by another platform, and " +
			"cannot be changed by this call, so no checkout was " +
			"created; that checkout must be finished or canceled by " +
			"whoever started it before this cart can be checked out here"}

	// errCartUnread: the cart's record names a checkout this server
	// cannot read. Like errCheckoutGone it never says to start another
	// checkout: an order may exist.
	errCartUnread = &mcptool.ToolError{Code: mcptool.CodeConflict,
		Hint: "this cart is held by a checkout whose records this " +
			"server cannot read, so an order may exist and no checkout " +
			"was created: do not start another checkout for it before " +
			"checking. If you hold that checkout's id, read it with " +
			"get_checkout; if you hold its order id, read the order " +
			"with get_order or track_order; if not, ask the shopper for " +
			"the store's order confirmation. The cart is released when " +
			"that checkout's record ends"}

	// errCartBusy: in every round another request was at work on the
	// cart's checkout, or had just changed it.
	errCartBusy = &mcptool.ToolError{Code: mcptool.CodeConflict,
		Hint: "another request is at work on this cart's checkout, so " +
			"this call created none; send it again shortly"}
)

// applyRegion settles the delivery region whenever the request set the
// delivery: its country decides whether one is needed at all, and the
// destination's address_region names it.
func (h *handlers) applyRegion(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	delivered bool,
) error {
	if !delivered {
		return nil
	}
	if err := checkout.ResolveRegion(
		ctx, h.deps.Django, t, s, s.Fulfillment.RegionID,
	); err != nil {
		return upstreamErr(err,
			"the delivery regions are unavailable; retry shortly")
	}
	return nil
}

// applyPayment honours a payment selection: it changes the totals
// through the method's fee. A hosted pay_way_id and an advertised
// instrument are mutually exclusive. A request that names neither keeps
// the selection the checkout holds: the response does not carry it, so
// a platform sending the checkout back cannot repeat it.
func (h *handlers) applyPayment(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	in *UCPPaymentChoice, neg ucp.Negotiated,
) error {
	if err := in.applyHostedSelection(s, neg); err != nil {
		return err
	}
	if in.Payment == nil || len(in.Payment.Instruments) == 0 {
		return nil
	}
	payWayID, err := resolvePayWay(ctx, h.deps.Django, t, s.Fulfillment,
		s.PayWayID, in.Payment.Instruments)
	if err != nil {
		return err
	}
	s.PayWayID = payWayID
	return nil
}

// payWayForDelivery keeps the method an update did not name one the
// checkout's delivery is offered. Django offers some methods only for
// some carriers and kinds, and an instrument is resolved for the delivery
// the checkout had then: a held method the delivery does not offer is
// replaced by the one that settles the same instrument for it, as naming
// the instrument again would select, or dropped when none does, and the
// checkout then asks for one. Kept, it would fail the order at
// completion, and a complete that names the same instrument would read
// as a change of method. The response shows the fee and total it brings.
func (h *handlers) payWayForDelivery(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
) error {
	if s.PayWayID <= 0 {
		return nil
	}
	pws, err := checkout.PayWaysFor(ctx, h.deps.Django, t, s.Fulfillment)
	if err != nil {
		return upstreamErr(err, "payment methods are unavailable")
	}
	if slices.ContainsFunc(pws, func(pw django.PayWay) bool {
		return pw.Active && pw.ID == s.PayWayID
	}) {
		return nil
	}
	held, err := h.deps.Django.PayWayByID(
		ctx, t.Domain, t.DefaultLocale, s.PayWayID)
	switch {
	case errors.Is(err, django.ErrNotFound):
		// The store offers it for no delivery any more.
		s.PayWayID = 0
		return nil
	case err != nil:
		return upstreamErr(err, "payment methods are unavailable")
	}
	s.PayWayID = 0
	if kind, ok := ucp.InstrumentTypeFor(held.ProviderCode); ok {
		s.PayWayID = instrumentPayWay(pws, kind, 0)
	}
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
	now, err := h.deps.Checkout.Load(
		ctx, t.SchemaName, ucpCaller(ctx), s.ID)
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
	return h.negotiated(ctx, in.Meta, false,
		func(t *tenant.Tenant, neg ucp.Negotiated) (
			*mcp.CallToolResult, CheckoutResult, error,
		) {
			return h.onSession(ctx, t, in.ID,
				func(s *checkout.Session) sessionAnswer {
					return h.updateSession(ctx, t, s, in, neg)
				},
				func(now *checkout.Session) toolResult {
					return h.updateStored(ctx, now)
				})
		})
}

// updateStored answers an update whose checkout another request stored
// while it ran.
func (h *handlers) updateStored(
	ctx context.Context, now *checkout.Session,
) toolResult {
	if err := h.closedTo(ctx, now, "updated"); err != nil {
		return refusedBy(err).answer
	}
	// Still open: this update's cart writes may be made.
	return refusedBy(withHint(errCheckoutChanged, checkoutNotUpdated)).answer
}

// updateSession is update_checkout once its checkout is loaded.
func (h *handlers) updateSession(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	in UpdateCheckoutIn, neg ucp.Negotiated,
) sessionAnswer {
	if err := h.closedTo(ctx, s, "updated"); err != nil {
		return refusedBy(err)
	}
	// Update replaces the checkout ("the entire checkout resource"): the
	// submitted lines replace the cart's, the buyer is what the request
	// carries, and so are the discount codes. What the response does not
	// carry (the gateway's own fulfillment member, the payment selection)
	// is kept when omitted (applyTo, applyPayment).
	lines, err := in.Checkout.lines()
	if err != nil {
		return refusedBy(err)
	}
	if len(lines) == 0 {
		return refusedBy(invalidInput(
			"checkout.line_items is required and must not be empty; " +
				"cancel the checkout to abandon it"))
	}
	// Every lookup and refusal before the first cart write: a refused
	// update must leave the cart and the stored session agreeing.
	// Nothing here is saved until the end.
	delivered, err := in.Checkout.applyTo(s, neg)
	if err != nil {
		return refusedBy(err)
	}
	if err := h.applyRegion(ctx, t, s, delivered); err != nil {
		return refusedBy(err)
	}
	err = h.applyPayment(ctx, t, s, &in.Checkout.UCPPaymentChoice, neg)
	if err != nil {
		return refusedBy(err)
	}
	if !in.Checkout.namesPayment() {
		if err := h.payWayForDelivery(ctx, t, s); err != nil {
			return refusedBy(err)
		}
	}
	codes, hasCodes := in.Checkout.replacedDiscountCodes(neg)
	// The lookups above can outlast the lock. A checkout another call
	// stored meanwhile, a completion included, gets none of this
	// update's cart writes; finish answers by what is stored.
	if err := h.deps.Checkout.Current(ctx, s); err != nil {
		return refusedBy(saveFailed(err))
	}
	if err := checkout.SyncCartLines(
		ctx, h.deps.Django, t, s.Protocol, s.CartID, lines,
	); err != nil {
		return refusedBy(withHint(upstreamErr(err, cartGone),
			checkoutNotUpdated))
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
	return h.negotiated(ctx, in.Meta, true,
		func(t *tenant.Tenant, neg ucp.Negotiated) (
			*mcp.CallToolResult, CheckoutResult, error,
		) {
			return h.onSession(ctx, t, in.ID,
				func(s *checkout.Session) sessionAnswer {
					return h.completeSession(ctx, t, s, in, neg)
				},
				func(now *checkout.Session) toolResult {
					return h.completeClosed(ctx, t, now, neg)
				})
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
	// The escalation resolves through the order, never through a second
	// complete, except to retry a payment link that could not be
	// created, for an order that already exists and can still be paid.
	if s.Status == checkout.StatusRequiresEscalation && s.PaymentURL == "" {
		err := h.deps.Flow.ResumePayment(ctx, t, s)
		if errors.Is(err, checkout.ErrPaymentWindowClosed) {
			return refusedBy(paymentClosedErr(s))
		}
		if errors.Is(err, checkout.ErrPaymentAwaitsStore) {
			// The checkout says what the payment awaits.
			return fromSnapshot(render)
		}
		if err != nil {
			return refusedBy(paymentLinkErr(s))
		}
		err = h.deps.Checkout.Save(ctx, s)
		if errors.Is(err, checkout.ErrSessionChanged) {
			err = h.keepLink(ctx, t, s)
		}
		if err != nil {
			return refusedBy(saveFailed(err))
		}
		return afterSave(render)
	}
	// A checkout whose order went unanswered, or whose completion ended
	// before it stored what happened, has its order sent again with its
	// id, and Django answers the order it placed, if it placed one. Its
	// payment is the one it was placed with, whatever this call names.
	// No completion claim is taken: Django keeps one order per checkout,
	// and the in-progress save lets one call through.
	if h.deps.Flow.Resumable(s) {
		placement, err := h.deps.Flow.Resume(ctx, t, s)
		return h.placed(ctx, s, placement, err, render)
	}
	if closed := h.completeClosed(ctx, t, s, neg); closed != nil {
		return fromSnapshot(closed)
	}

	// Complete is a command, not an update: it places the checkout as it
	// stands, and never at a total the checkout did not show. The
	// submitted instrument is resolved, which refuses one the store
	// cannot honour. Then:
	//   - it selects the method the checkout holds: the order is placed;
	//   - it selects another: the fee, and the total, would change at
	//     placement;
	//   - the checkout holds none (the spec requires `payment` on a
	//     complete, so a platform may first name its instrument here):
	//     the method is taken when it leaves the total as the checkout
	//     showed it, and not when it adds a fee.
	// A change is answered incomplete, and nothing is stored or placed.
	held := s.PayWayID
	if err := h.applyPayment(ctx, t, s, &in.Checkout, neg); err != nil {
		return refusedBy(err)
	}
	// The snapshot stays the stored checkout until this call saves it.
	named := s.PayWayID
	s.PayWayID = held
	placedNothing := func(m ucp.Message) sessionAnswer {
		return fromSnapshot(func() (*mcp.CallToolResult, CheckoutResult,
			error) {
			res, out, err := h.checkoutResult(ctx, t, s, neg)
			return incompleteBy(res, out, err, m)
		})
	}
	switch {
	case named <= 0:
		return refusedBy(errNoPaymentMethod)
	case named != held && held > 0:
		return placedNothing(paymentChanged())
	}
	if named != held {
		fee, err := h.firstMethodFee(ctx, t, s, named)
		if err != nil {
			return refusedBy(err)
		}
		if fee != nil {
			return placedNothing(*fee)
		}
	}

	claim, err := h.deps.Checkout.ClaimCompletion(
		ctx, t.SchemaName, s.ID, in.Meta.IdempotencyKey)
	switch {
	case errors.Is(err, checkout.ErrCompletionInProgress):
		return refusedBy(errCompletionInFlight)
	case errors.Is(err, checkout.ErrInvalidIdempotencyKey):
		// MetaIn.validate refuses such a key before anything is read.
		return refusedBy(errIdempotencyKeyForm)
	case err != nil:
		return refusedBy(errCheckoutUnavailable)
	}
	if claim == nil {
		// A prior attempt with this key finished. What it made of the
		// checkout is what is stored now, which finish answers with:
		// this call's snapshot may predate it.
		return fromSnapshot(render)
	}

	// The method a complete names first is the buyer's choice whether or
	// not the call places the order. It is stored now, on a checkout
	// that is still open (completeClosed), because a refused completion
	// stores nothing: a later complete that names none would otherwise
	// find the checkout without it. The order is placed from the cart the
	// checkout showed: the digest holds no pay way.
	if named != held {
		s.PayWayID = named
		s.Recompute()
		if err := h.deps.Checkout.Save(ctx, s); err != nil {
			h.settleCompletion(ctx, s, claim, checkout.NothingPlaced)
			return refusedBy(saveFailed(err))
		}
	}

	placement, err := h.deps.Flow.Complete(ctx, t, s)
	h.settleCompletion(ctx, s, claim, placement)
	return h.placed(ctx, s, placement, err, render)
}

// placed answers a completion by what Flow.Complete or Flow.Resume did.
func (h *handlers) placed(
	ctx context.Context, s *checkout.Session, placement checkout.Placement,
	err error, render toolResult,
) sessionAnswer {
	if placement == checkout.NothingPlaced {
		// The street line Django refused is the address's to correct:
		// the answer is the checkout, stored incomplete, with the error
		// at the destination's street_address ("incomplete for a
		// recoverable change").
		if errors.As(err, new(*django.StreetRefusedError)) {
			return fromSnapshot(render)
		}
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

// stillNeeded names what the checkout still needs, as its messages
// detail it.
func stillNeeded(s *checkout.Session) string {
	needs := s.Missing()
	names := make([]string, 0, len(needs))
	for _, n := range needs {
		names = append(names, n.String())
	}
	return strings.Join(names, "; ")
}

// errNoPaymentMethod refuses a complete of a checkout that holds no
// payment method and names none.
var errNoPaymentMethod = &mcptool.ToolError{Code: mcptool.CodeMissing,
	Hint: "this checkout holds no payment method and this call names " +
		"none: submit one of the instruments the checkout advertises in " +
		"checkout.payment.instruments"}

// paymentChanged is the recoverable error of a complete whose payment
// selects another method than the checkout holds.
func paymentChanged() ucp.Message {
	return ucp.Message{Type: "error", Code: "payment_method_changed",
		Severity: "recoverable",
		Text: "checkout.payment selects a different payment method than " +
			"this checkout holds, which would change its fee and its " +
			"total, so no order was placed. Select that method with " +
			"update_checkout and confirm the new total, or complete " +
			"with checkout.payment {} to keep the method the checkout " +
			"holds."}
}

// firstMethodFee is the recoverable error of a complete that names the
// first payment method of a checkout, when the method adds a fee: the
// total would then differ from the one the checkout showed. It is nil
// for a method that leaves the total as it was (no fee for this cart,
// its free threshold included), which the complete takes.
//
// The fee is the render's own: checkout.ComputePricing on the checkout as
// it would be with the method, not a second calculation.
func (h *handlers) firstMethodFee(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session, payWayID int64,
) (*ucp.Message, error) {
	// ComputePricing reads past a pay way it cannot read; here a method
	// the store does not have is the caller's to hear about, not an
	// outage to retry.
	if _, err := h.deps.Django.PayWayByID(
		ctx, t.Domain, t.DefaultLocale, payWayID); err != nil {
		return nil, upstreamErr(err, "checkout.pay_way_id names no "+
			"payment method this store offers; see get_payment_methods")
	}
	with := *s
	with.PayWayID = payWayID
	pricing, _, err := checkout.ComputePricing(ctx, h.deps.Django, t, &with)
	switch {
	case err != nil && isCartGone(err):
		return nil, errCheckoutCartGone
	case err != nil:
		return nil, upstreamErr(err,
			"the checkout's totals could not be read; retry")
	case pricing.PayWayRefused != nil:
		return nil, &mcptool.ToolError{Code: mcptool.CodeUnsupported,
			Detail: pricing.PayWayRefused.Detail,
			Hint: "the payment method this call names is not offered for " +
				"this checkout's delivery; name another advertised " +
				"instrument, or select one with update_checkout"}
	case pricing.Refused() != nil:
		// Another choice the store refuses now (the delivery): named as
		// the completion names it, with what to change.
		r := pricing.Refused()
		return nil, completeErr(s, fmt.Errorf("%w: %s (%s)",
			checkout.ErrChoiceRefused, r.Detail, r.Code))
	case pricing.PayWay == nil:
		// A render goes on without a fee it could not read. A placement
		// does not: the fee decides whether the order is placed.
		return nil, upstreamErr(django.ErrUpstreamDown, "")
	case !pricing.HasPaymentFee:
		return nil, nil
	}
	amount := func(minor int64) string {
		return money.Format(minor) + " " + t.DefaultCurrency
	}
	return &ucp.Message{Type: "error", Code: "payment_fee_added",
		Severity: "recoverable",
		Text: fmt.Sprintf("this checkout holds no payment method, and "+
			"the one this call names adds a fee of %s, which makes the "+
			"total %s, so no order was placed. Select it with "+
			"update_checkout and confirm the new total, then complete "+
			"with the same instrument.",
			amount(pricing.PaymentFee), amount(pricing.Total))}, nil
}

// incompleteBy answers a complete that placed nothing because of what it
// submitted: the checkout as it is stored, as incomplete ("incomplete for
// a recoverable change"), with the error that says what to do.
func incompleteBy(
	res *mcp.CallToolResult, out CheckoutResult, err error, m ucp.Message,
) (*mcp.CallToolResult, CheckoutResult, error) {
	if err != nil {
		return res, out, err
	}
	out.checkout.Status = string(checkout.StatusIncomplete)
	out.checkout.Messages = append([]ucp.Message{m}, out.checkout.Messages...)
	return textResult("%s", m.Text), out, nil
}

// completeErr is the tool's answer for an error of Flow.Complete.
func completeErr(s *checkout.Session, err error) error {
	switch {
	case errors.Is(err, checkout.ErrOrderOutcomeUnknown):
		return errOutcomeUnknown
	case errors.Is(err, checkout.ErrPaymentSessionFailed):
		return paymentLinkErr(s)
	case errors.Is(err, checkout.ErrPaymentWindowClosed),
		errors.Is(err, checkout.ErrPaymentAwaitsStore):
		// The order is placed: never "placement failed".
		return paymentClosedErr(s)
	case errors.Is(err, checkout.ErrSessionNotSaved):
		return placedUnsavedErr(s.OrderUUID)
	case errors.Is(err, checkout.ErrUnavailable):
		return errCheckoutUnavailable
	case errors.As(err, new(*django.StockShortfall)):
		return stockShortfallErr(err)
	case errors.Is(err, django.ErrCartHasUnpaidOrder):
		return cartOrderUnpaidErr(err)
	// The answer shows no total: the next read shows, and stores, the
	// total now, so the same key keeps this answer until the platform has
	// read it, and completes at the total that read showed.
	case errors.Is(err, django.ErrTotalChanged):
		return &mcptool.ToolError{Code: django.CodeTotalChanged,
			Hint: "the store's total for this checkout is no longer the " +
				"one it showed, so no order was placed; read it with " +
				"get_checkout, confirm the total with the buyer, then " +
				"complete"}
	// As total_changed: the cart changed, at the same total or not.
	case errors.Is(err, django.ErrCartChanged):
		return &mcptool.ToolError{Code: django.CodeCartChanged,
			Hint: "this checkout's cart is no longer the one it showed " +
				"(its lines, discounts or delivery changed), so no order " +
				"was placed; read it with get_checkout, confirm it with " +
				"the buyer, then complete"}
	case errors.Is(err, checkout.ErrSessionChanged):
		return saveFailed(err)
	case errors.Is(err, checkout.ErrCompletionInProgress):
		return errCompletionInFlight
	case errors.Is(err, checkout.ErrChoiceRefused):
		return &mcptool.ToolError{Code: mcptool.CodeMissing,
			Detail: err.Error(),
			Hint: "the store refuses one of this checkout's choices for " +
				"its cart now; read it with get_checkout, which names it, " +
				"and change it with update_checkout, then complete"}
	case errors.Is(err, checkout.ErrTotalNotShown):
		return &mcptool.ToolError{Code: mcptool.CodeMissing,
			Hint: "this checkout has shown no total yet; read it with " +
				"get_checkout, confirm the total with the buyer, then " +
				"complete"}
	case errors.Is(err, checkout.ErrNotReady):
		return &mcptool.ToolError{Code: mcptool.CodeMissing,
			Hint: "this checkout is not ready to complete; still needed: " +
				stillNeeded(s) + ". Send them with update_checkout, then " +
				"complete"}
	case errors.Is(err, checkout.ErrPayWayUnavailable):
		return &mcptool.ToolError{Code: mcptool.CodeUnsupported,
			Hint: "the selected payment method is not offered for this " +
				"delivery; select another advertised instrument with " +
				"update_checkout, then complete"}
	case errors.Is(err, checkout.ErrPaymentMethodUnsupported):
		return &mcptool.ToolError{Code: mcptool.CodeUnsupported,
			Hint: "this payment method needs the buyer to pay on the " +
				"store's checkout; use an offline method (e.g. cash " +
				"on delivery), Viva card payment, or hand over " +
				"get_checkout_link instead"}
	}
	return upstreamErr(err, "order placement failed; the cart is unchanged")
}

// cartOrderUnpaidErr answers a completion Django refused because an
// earlier order from the checkout's cart still awaits its online payment:
// Django's code and sentence, and what to do. Nothing in the call was
// wrong, and the checkout stays ready. Once that order is paid the cart
// is emptied; once Django cancels it unpaid, this checkout can be placed.
// Django names no order, and neither does the answer.
func cartOrderUnpaidErr(err error) error {
	e := &mcptool.ToolError{Code: django.CodeCartHasUnpaidOrder,
		Hint: "an order placed from this checkout's cart already awaits " +
			"its payment, so this checkout placed no order: do not start " +
			"another checkout for this cart. The buyer pays that order, " +
			"or the store cancels it when its payment window ends; once " +
			"it is canceled, this checkout can be completed"}
	if apiErr, ok := errors.AsType[*django.APIError](err); ok {
		e.Detail = apiErr.Detail
	}
	return e
}

// placedUnsavedErr answers for an order that is placed while its
// checkout never recorded it: to the completion whose last save failed,
// and to every later call, which finds the checkout in progress and the
// order named beside it.
func placedUnsavedErr(orderUUID string) error {
	return &mcptool.ToolError{Code: mcptool.CodeOrderPlaced,
		Hint: "the order was placed but checkout state could not be " +
			"saved; track it with track_order using orderUuid " +
			orderUUID + ", and do not complete again or start a new " +
			"checkout"}
}

// settleCompletion spends or releases a completion claim by what the
// attempt did about the order. It runs even when the call's context has
// ended (a platform that hung up, a shutdown), on
// checkout.Store.Settling: a retry must find what this attempt did, not
// a claim left in flight.
//
// A claim that lapsed while the attempt ran may be another call's by
// now, and is left to it. The answer does not depend on it: what this
// attempt did about the order stands, and the session says so to
// whoever holds the key.
func (h *handlers) settleCompletion(
	ctx context.Context, s *checkout.Session, claim *checkout.Claim,
	placement checkout.Placement,
) {
	settleCtx, cancel := h.deps.Checkout.Settling(ctx)
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
		return &mcptool.ToolError{Code: mcptool.CodeCheckoutClosed,
			Hint: fmt.Sprintf("checkout %s is canceled and cannot be %s",
				s.ID, action)}
	}
	return &mcptool.ToolError{Code: mcptool.CodeCheckoutClosed,
		Hint: fmt.Sprintf("checkout %s already placed order %s and cannot "+
			"be %s here; the order is managed by the store, and "+
			"get_order or track_order reads it", s.ID, s.OrderUUID,
			action)}
}

// errOutcomeUnknown answers a checkout whose order the store has not
// confirmed or denied, once no completion of it is running. Completing
// it again asks the store again with the checkout's id
// (checkout.Flow.Resume), and the store answers the order it placed, if
// it placed one: that never orders twice, while another checkout could.
// A new key: the complete that left the outcome unknown spent its own,
// and once the store refuses the order (the checkout ready again) that
// key only reads the checkout back.
var errOutcomeUnknown = &mcptool.ToolError{
	Code:       mcptool.CodeOrderOutcomeUnknown,
	RetryAfter: mcptool.UnavailableRetryAfter,
	Hint: "the store has not confirmed whether this checkout's order " +
		"was placed. Call complete_checkout on this checkout again " +
		"shortly, with a new meta.idempotency-key: the store answers " +
		"with the order this checkout placed, if it placed one, and " +
		"never places a second, so do not start another checkout for " +
		"this purchase"}

// pending answers a call that finds the checkout's order being placed.
// An order the completion placed and could not save is known (the
// checkout or the store names it): that is the answer. Else, while the
// completion can still run, to check again shortly; past that
// (Flow.CompletionDeadline, as ACP judges it), that its outcome is not
// recorded, which a complete asks the store for (Flow.Resume).
func (h *handlers) pending(ctx context.Context, s *checkout.Session) error {
	if order := h.deps.Flow.PlacedOrder(ctx, s); order != "" {
		return placedUnsavedErr(order)
	}
	if time.Until(h.deps.Flow.CompletionDeadline(s)) > 0 {
		return errCompletionInFlight
	}
	return errOutcomeUnknown
}

// guidance is what an answer tells the agent to do, as a sentence for a
// result's text: a ToolError's hint, or a plain error's text.
func guidance(err error) string {
	if te, ok := errors.AsType[*mcptool.ToolError](err); ok {
		return te.Hint
	}
	return err.Error()
}

// errCheckoutGone answers a checkout id no session is stored for. The id
// does not say why: the checkout was never completed and expired, it
// placed an order and its session is no longer kept, or the id is wrong.
// So the answer never says only to start another: an agent that did
// would order a second time in the second case.
var errCheckoutGone = &mcptool.ToolError{Code: mcptool.CodeNotFound,
	Hint: "that checkout no longer exists here. It may never have been " +
		"completed, it may have placed an order whose checkout is no " +
		"longer kept, or the id may be wrong, so an order may exist: " +
		"do not start another checkout before checking. If you hold " +
		"its order id, read the order with get_order or track_order; " +
		"if not, ask the shopper for the store's order confirmation. " +
		"Only if it was never completed, start a new one with " +
		"create_checkout"}

// errCheckoutBusy answers a call on a checkout another request holds
// the lock of: nothing of this call was done.
var errCheckoutBusy = &mcptool.ToolError{Code: mcptool.CodeConflict,
	Hint: "another request is at work on this checkout, so this call " +
		"did nothing; send it again shortly"}

// errCompletionInFlight answers a call that finds the checkout's order
// being placed, while that completion can still be running: an order
// may exist by the time it ends.
var errCompletionInFlight = &mcptool.ToolError{Code: mcptool.CodeConflict,
	Hint: "a completion of this checkout is in progress or unconfirmed, " +
		"so an order may exist: do not complete it again or start " +
		"another checkout; read it with get_checkout shortly"}

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
	// cartGone is a checkout's cart Django no longer serves for its id.
	// Django removes a guest cart that an order emptied or a sign-in
	// merged, so the shopper may have checked it out on the store's own
	// site: an order may exist, and this is never only "start another".
	cartGone = "the checkout's cart no longer exists: the shopper may " +
		"have checked it out on the store's site, so an order may " +
		"exist. Check with the shopper, or the store's order " +
		"confirmation, before starting another checkout with " +
		"create_checkout"
	// cartIDGone is cartGone for a create naming a cart: the caller has
	// no checkout on it yet.
	cartIDGone = "that cart no longer exists: the shopper may have " +
		"checked it out on the store's site, so an order may exist. " +
		"Check with the shopper before building another cart with " +
		"create_cart"
)

// errCheckoutCartGone answers a checkout without an order whose cart
// Django no longer serves (checkoutResult). It is shared and never
// changed: withHint copies it.
var errCheckoutCartGone = &mcptool.ToolError{Code: mcptool.CodeNotFound,
	Hint: cartGone}

// unrenderedErr is the answer for an order that exists while its
// checkout is not rendered: the agent must read it again, not order
// again.
func unrenderedErr(s *checkout.Session) error {
	return &mcptool.ToolError{Code: mcptool.CodeOrderPlaced,
		Hint: fmt.Sprintf("order %s is placed, but the checkout could not "+
			"be rendered; read it again with get_checkout, and do not "+
			"start a new checkout", s.OrderUUID)}
}

// paymentLinkErr is the truthful answer when the order exists but its
// hosted payment link could not be created: saying the order failed
// would send the buyer to check out again and order twice.
func paymentLinkErr(s *checkout.Session) error {
	return &mcptool.ToolError{Code: mcptool.CodePaymentLinkFailed,
		Hint: fmt.Sprintf("order %s is placed, but its payment link could "+
			"not be created; call complete_checkout again for checkout %s "+
			"to retry the payment link — do not start a new checkout",
			s.OrderUUID, s.ID)}
}

// keepLink stores the payment link a complete minted over its checkout
// as stored since: a read stores what it read from the order (rendered),
// and may have stored it while the link was being minted. The link is
// kept only on that checkout still awaiting its payment with no link;
// anything else is the other request's, and is ErrSessionChanged. A link
// the checkout never stored would be minted again by the next complete.
func (h *handlers) keepLink(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
) error {
	now, err := h.deps.Checkout.Load(ctx, t.SchemaName, ucpCaller(ctx), s.ID)
	if err != nil {
		return err
	}
	if now.Status != checkout.StatusRequiresEscalation ||
		now.PaymentURL != "" || now.OrderUUID != s.OrderUUID {
		return checkout.ErrSessionChanged
	}
	now.PaymentURL = s.PaymentURL
	if err := h.deps.Checkout.Save(ctx, now); err != nil {
		return err
	}
	*s = *now
	return nil
}

// paymentClosedErr answers a retry of the payment link of an order the
// store no longer takes a payment for: its time to pay ran out, or it
// is paid or no longer open. That is final: no link is minted for it.
func paymentClosedErr(s *checkout.Session) error {
	return &mcptool.ToolError{Code: mcptool.CodeCheckoutClosed,
		Hint: fmt.Sprintf("order %s can no longer be paid here: its time "+
			"to pay ran out, or it is already paid, or its payment awaits "+
			"the store. Do not complete checkout %s again or start another "+
			"checkout: read it with get_checkout, which says whether the "+
			"order is paid, awaits the store, or will be canceled unpaid "+
			"by the checkout's expires_at", s.OrderUUID, s.ID)}
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
	return &mcptool.ToolError{Code: mcptool.CodeOutOfStock,
		Hint: fmt.Sprintf("not enough stock: %s — adjust quantities and "+
			"retry", strings.Join(lines, "; "))}
}

// errIDRequired refuses a UCP call whose id, of the checkout or the
// order, is empty.
var errIDRequired = invalidInput("id is required")

func (h *handlers) lockedSession(
	ctx context.Context, t *tenant.Tenant, id string,
) (*checkout.Session, func(), error) {
	if id == "" {
		return nil, nil, errIDRequired
	}
	s, release, err := h.deps.Checkout.LockSession(
		ctx, t.SchemaName, ucpCaller(ctx), id)
	switch {
	case errors.Is(err, checkout.ErrLocked):
		return nil, nil, errCheckoutBusy
	case errors.Is(err, checkout.ErrNotFound):
		return nil, nil, errCheckoutGone
	case err != nil:
		return nil, nil, errCheckoutUnavailable
	}
	return s, release, nil
}

func (h *handlers) checkoutResult(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	neg ucp.Negotiated,
) (*mcp.CallToolResult, CheckoutResult, error) {
	var zero CheckoutResult
	payload, err := h.rendered(ctx, t, s, neg)
	if _, ok := errors.AsType[*mcptool.ToolError](err); ok {
		return nil, zero, err
	}
	if err != nil {
		switch {
		case s.OrderUUID != "":
			return nil, zero, unrenderedErr(s)
		case s.MayHaveOrder():
			// Being placed, or never confirmed: its cart may have gone
			// with the order, so it is answered as what it is, not as a
			// checkout to read again until it renders.
			return nil, zero, h.pending(ctx, s)
		case isCartGone(err):
			// Without an order the render reads the cart, and its other
			// reads (pay way, shipping options) name the cart too. Django
			// no longer serves it, and no retry reaches it again.
			return nil, zero, errCheckoutCartGone
		}
		return nil, zero, upstreamErr(err,
			"checkout state could not be rendered; retry")
	}

	var text string
	switch s.Status {
	case checkout.StatusIncomplete:
		text = fmt.Sprintf(
			"Checkout %s is incomplete — still needed: %s.",
			s.ID, stillNeeded(s))
	case checkout.StatusReadyForComplete:
		text = fmt.Sprintf(
			"Checkout %s is ready — call complete_checkout to place the "+
				"order.", s.ID)
	case checkout.StatusRequiresEscalation:
		// The message says whether, and by when, the store decides it.
		text = fmt.Sprintf(
			"Order %s is placed and awaits its payment: %s "+
				"(continue_url: %s). The checkout completes when the "+
				"payment lands, or is canceled with the order; it can be "+
				"read until %s.",
			s.OrderUUID, firstError(payload).Text, payload.ContinueURL,
			payload.ExpiresAt)
	case checkout.StatusCompleted:
		text = fmt.Sprintf(
			"Order %s is placed and confirmed. Track it with track_order.",
			s.OrderUUID)
	case checkout.StatusCompleteInProgress:
		// The guidance every other tool gives for this state: in flight,
		// the order it placed, or an outcome that was never recorded.
		text = guidance(h.pending(ctx, s))
	default:
		text = fmt.Sprintf("Checkout %s status: %s.", s.ID, s.Status)
	}
	// A choice the store refuses now makes a ready checkout render
	// incomplete; its error says what to change.
	if s.Status == checkout.StatusReadyForComplete &&
		payload.Status == string(checkout.StatusIncomplete) {
		text = fmt.Sprintf("Checkout %s is incomplete — %s", s.ID,
			firstError(payload).Text)
	}
	return textResult("%s", text), CheckoutResult{checkout: payload}, nil
}

// rendered builds the checkout and stores what the render read from its
// order (a status the order decided, its expiry) and what it shows, its
// total and cart digest (Session.Shown), before giving it: a checkout
// publishes only what is stored, so no later read gives an earlier
// expires_at, and its order is placed at the total and from the cart it
// showed. A read (unlocked) that changed only what it shows stores it by
// taking the lock, and not while another call holds
// it (checkout.Store.StoreShown). The save is a compare-and-set: a
// checkout another request stored meanwhile is read again and rendered
// once more. A placed checkout whose state cannot be stored is answered
// as one placed and not rendered; an open one as unavailable, or as busy
// or changed when another call holds or stored it: a total that is not
// stored is never published, since its order would be placed at the one
// stored before.
func (h *handlers) rendered(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	neg ucp.Negotiated,
) (*ucp.Checkout, error) {
	for again := true; ; again = false {
		status, expires, shown := s.Status, s.ExpiresAt, s.Shown
		payload, err := h.deps.UCP.BuildCheckout(ctx, t, s,
			neg.Declaration(ucp.CapabilityCheckout))
		if err != nil ||
			(s.Status == status && s.ExpiresAt.Equal(expires) &&
				s.Shown == shown) {
			return payload, err
		}
		// A read changed only what it shows: it stores it under
		// the lock, which another call may hold. A total that cannot be
		// stored is not published: its order would be placed at the one
		// stored before.
		if read, _ := ctx.Value(unlockedKey{}).(bool); read &&
			s.Status == status && s.ExpiresAt.Equal(expires) {
			err = h.deps.Checkout.StoreShown(ctx, ucpCaller(ctx), s)
			switch {
			case err == nil:
				return payload, nil
			case errors.Is(err, checkout.ErrLocked):
				return nil, errCheckoutBusy
			case errors.Is(err, checkout.ErrNotFound):
				return nil, errCheckoutGone
			case !errors.Is(err, checkout.ErrSessionChanged):
				return nil, errCheckoutUnavailable
			}
		} else if err = h.deps.Checkout.Save(ctx, s); err == nil {
			return payload, nil
		}
		// An open checkout has no order to speak of: one whose state
		// cannot be stored is unavailable, one that other requests keep
		// storing is answered as changed.
		open := s.Open()
		if open && !errors.Is(err, checkout.ErrSessionChanged) {
			h.deps.Log.WarnContext(ctx, "checkout: shown total not stored",
				slog.String("session", s.ID),
				slog.String("error", err.Error()))
			return nil, errCheckoutUnavailable
		}
		if !again || !errors.Is(err, checkout.ErrSessionChanged) {
			h.deps.Log.WarnContext(ctx, "checkout: order state not stored",
				slog.String("session", s.ID),
				slog.String("error", err.Error()))
			if open {
				return nil, saveFailed(err)
			}
			return nil, unrenderedErr(s)
		}
		now, err := h.deps.Checkout.Load(
			ctx, t.SchemaName, ucpCaller(ctx), s.ID)
		switch {
		case errors.Is(err, checkout.ErrNotFound):
			return nil, errCheckoutGone
		case err != nil && open:
			return nil, errCheckoutUnavailable
		case err != nil:
			return nil, unrenderedErr(s)
		}
		*s = *now
	}
}

// firstError is a checkout's first error message: what a checkout
// awaiting its payment says the buyer has to do.
func firstError(c *ucp.Checkout) ucp.Message {
	for _, m := range c.Messages {
		if m.Type == "error" {
			return m
		}
	}
	return ucp.Message{}
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
	return h.negotiated(ctx, in.Meta, false,
		func(t *tenant.Tenant, neg ucp.Negotiated) (
			*mcp.CallToolResult, CheckoutResult, error,
		) {
			var zero CheckoutResult
			if in.ID == "" {
				return nil, zero, errIDRequired
			}
			s, err := h.deps.Checkout.Load(
				ctx, t.SchemaName, ucpCaller(ctx), in.ID)
			switch {
			case errors.Is(err, checkout.ErrNotFound):
				return nil, zero, errCheckoutGone
			case err != nil:
				return nil, zero, errCheckoutUnavailable
			}
			return h.checkoutResult(unlocked(ctx), t, s, neg)
		})
}

// unlockedKey marks a call that renders a checkout without holding its
// lock: a read.
type unlockedKey struct{}

// unlocked marks ctx as a call that does not hold the checkout's lock: a
// read. It stores the total an open checkout shows only by taking the
// lock (checkout.Store.StoreShown), so the update or completion holding
// it never loses its save to a read.
func unlocked(ctx context.Context) context.Context {
	return context.WithValue(ctx, unlockedKey{}, true)
}

// cancelCheckout abandons a session the buyer is no longer pursuing.
//
// A terminal session is re-rendered rather than refused: cancel is
// idempotent by nature, and a platform retrying after a lost response
// must not receive an error for work already done.
//
// The spec requires meta.idempotency-key on cancel, so it is validated
// (MetaIn.validate); it is never used after that, because cancel is
// idempotent by nature.
func (h *handlers) cancelCheckout(
	ctx context.Context, _ *mcp.CallToolRequest, in CancelCheckoutIn,
) (*mcp.CallToolResult, CheckoutResult, error) {
	return h.negotiated(ctx, in.Meta, true,
		func(t *tenant.Tenant, neg ucp.Negotiated) (
			*mcp.CallToolResult, CheckoutResult, error,
		) {
			return h.onSession(ctx, t, in.ID,
				func(s *checkout.Session) sessionAnswer {
					return h.cancelSession(ctx, t, s, neg)
				},
				func(now *checkout.Session) toolResult {
					return h.cancelClosed(ctx, t, now, neg)
				})
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
