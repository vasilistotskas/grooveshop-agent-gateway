package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
	// CartID is an additive member UCP does not model: a canonical
	// caller sends line_items. Consumers ignore members they do not
	// recognise, so carrying it costs conformance nothing while keeping
	// the cart tools usable as a path into checkout. (Order webhooks are
	// discovered from the platform profile, never taken from a request.)
	CartID string `json:"cart_id,omitempty" jsonschema:"an existing cart from the cart tools; omit when sending line_items"`
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
	}

	s := checkout.NewSession(
		t.SchemaName, t.Domain, checkout.ProtocolUCP, in.CartID)
	// The platform's order-event endpoint, from its profile's negotiated
	// order capability (vetted by negotiate); none when it negotiated
	// no order capability.
	s.WebhookURL = neg.OrderWebhookURL()
	// Every lookup and refusal before the first cart write, which here
	// may be creating the cart: a region or payment the store refuses
	// leaves no cart behind. A line sync failing after it does.
	in.Checkout.applyTo(s)
	if err := h.applyRegion(ctx, t, s, &in.Checkout); err != nil {
		return nil, zero, err
	}
	if err := h.applyPayment(ctx, t, s, &in.Checkout, neg); err != nil {
		return nil, zero, err
	}

	if s.CartID == "" {
		c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, "")
		if err != nil {
			return nil, zero, upstreamErr(err,
				"the cart service is unavailable")
		}
		s.CartID = c.UUID
		if err := checkout.SyncCartLines(
			ctx, h.deps.Django, t, checkout.ProtocolUCP, s.CartID, lines,
		); err != nil {
			return nil, zero, withHint(upstreamErr(err,
				"the cart made for this checkout could not be reached; "+
					"start again"), checkoutNotCreated)
		}
	}
	codes, hasCodes := in.Checkout.discountCodes()
	if hasCodes {
		if err := checkout.ApplyDiscountCodes(
			ctx, h.deps.Django, t, s, codes,
		); err != nil {
			return nil, zero, withHint(upstreamErr(err,
				"the cart was not found; start a new checkout with "+
					"line_items"), checkoutNotCreated)
		}
	}
	s.Recompute()
	if err := h.deps.Checkout.Save(ctx, s); err != nil {
		return nil, zero, errCheckoutUnavailable
	}
	res, out, err := h.checkoutResult(ctx, t, s, neg)
	return h.discountAnnotated(res, out, err, s, hasCodes)
}

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

	if !s.Open() {
		return nil, zero, fmt.Errorf(
			"checkout %s is %s and can no longer be updated",
			s.ID, s.Status)
	}
	// line_items is full-state on update: the submitted list replaces
	// the cart's lines. Ignoring it would complete the old order.
	lines, err := in.Checkout.lines()
	if err != nil {
		return nil, zero, err
	}
	if in.Checkout.LineItems != nil && len(lines) == 0 {
		return nil, zero, errors.New(
			"checkout.line_items must not be empty; cancel the " +
				"checkout to abandon it")
	}
	// Every lookup and refusal before the first cart write: a refused
	// update must leave the cart and the stored session agreeing.
	// Nothing here is saved until the end.
	in.Checkout.applyTo(s)
	if err := h.applyRegion(ctx, t, s, &in.Checkout); err != nil {
		return nil, zero, err
	}
	if err := h.applyPayment(ctx, t, s, &in.Checkout, neg); err != nil {
		return nil, zero, err
	}
	if in.Checkout.LineItems != nil {
		if err := checkout.SyncCartLines(
			ctx, h.deps.Django, t, s.Protocol, s.CartID, lines,
		); err != nil {
			return nil, zero, withHint(upstreamErr(err, cartGone),
				checkoutNotUpdated)
		}
	}
	codes, hasCodes := in.Checkout.discountCodes()
	if hasCodes {
		if err := checkout.ApplyDiscountCodes(
			ctx, h.deps.Django, t, s, codes,
		); err != nil {
			return nil, zero, withHint(upstreamErr(err, cartGone),
				checkoutNotUpdated)
		}
	}
	s.Recompute()
	if err := h.deps.Checkout.Save(ctx, s); err != nil {
		return nil, zero, errCheckoutUnavailable
	}
	res, out, err := h.checkoutResult(ctx, t, s, neg)
	return h.discountAnnotated(res, out, err, s, hasCodes)
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

	switch s.Status {
	case checkout.StatusCompleted:
		return h.checkoutResult(ctx, t, s, neg)
	case checkout.StatusCompleteInProgress:
		return nil, zero, checkout.ErrCompletionInProgress
	case checkout.StatusRequiresEscalation:
		// The escalation resolves through the order event, never through
		// a second complete — except to retry a payment link that could
		// not be created, for an order that already exists.
		if err := h.deps.Flow.ResumePayment(ctx, t, s); err != nil {
			return nil, zero, paymentLinkErr(s)
		}
		if err := h.deps.Checkout.Save(ctx, s); err != nil {
			return nil, zero, errCheckoutUnavailable
		}
		return h.checkoutResult(ctx, t, s, neg)
	}

	// The submitted instrument decides how this order settles. Resolving
	// it here — rather than trusting a pay-way set earlier — is what
	// makes the payment object authoritative, and it rejects an
	// instrument the store cannot honour instead of placing an order the
	// buyer has no way to pay for.
	payWay, status := s.PayWayID, s.Status
	if err := h.applyPayment(ctx, t, s, &in.Checkout, neg); err != nil {
		return nil, zero, err
	}
	if s.PayWayID <= 0 {
		return nil, zero, errors.New(
			"checkout.payment.instruments is required to complete: " +
				"submit one of the instruments the checkout advertised")
	}
	s.Recompute()

	idemKey := in.Meta.IdempotencyKey
	claimed, err := h.deps.Checkout.ClaimCompletion(
		ctx, t.SchemaName, s.ID, idemKey)
	if err != nil {
		if errors.Is(err, checkout.ErrCompletionInProgress) {
			return nil, zero, err
		}
		return nil, zero, errCheckoutUnavailable
	}
	if !claimed {
		// A prior attempt finished; render the session's current state.
		return h.checkoutResult(ctx, t, s, neg)
	}

	// What this call submitted is the buyer's latest choice whether or
	// not the call places the order. It is stored now, because a refused
	// completion stores nothing: a later complete that omits the payment
	// would otherwise place the order with the choice before this one.
	//
	// Only under the claim: the payment lookup above can outlast the
	// session lock, and a retry of this key may have placed the order
	// meanwhile. This snapshot must not be written over that session.
	if s.Open() && (s.PayWayID != payWay || s.Status != status) {
		if err := h.deps.Checkout.Save(ctx, s); err != nil {
			h.settleCompletion(ctx, t, s, idemKey, checkout.NothingPlaced)
			return nil, zero, errCheckoutUnavailable
		}
	}

	placement, err := h.deps.Flow.Complete(ctx, t, s)
	h.settleCompletion(ctx, t, s, idemKey, placement)
	switch {
	case err == nil:
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
			return nil, zero, unrenderedErr(s)
		}
		return h.checkoutResult(ctx, t, s, neg)
	case errors.Is(err, checkout.ErrOrderOutcomeUnknown):
		return nil, zero, errors.New(
			"the store did not confirm whether the order was placed; do " +
				"not retry or start a new checkout — the buyer receives " +
				"a confirmation email if it was, and track_order finds it")
	case errors.Is(err, checkout.ErrPaymentSessionFailed):
		return nil, zero, paymentLinkErr(s)
	case errors.Is(err, checkout.ErrSessionNotSaved):
		return nil, zero, errors.New(
			"the order was placed but checkout state could not be saved; " +
				"track it with track_order using orderUuid " + s.OrderUUID)
	case errors.As(err, new(*django.StockShortfall)):
		return nil, zero, stockShortfallErr(err)
	case errors.Is(err, checkout.ErrNotReady),
		errors.Is(err, checkout.ErrCompletionInProgress):
		return nil, zero, err
	case errors.Is(err, checkout.ErrPayWayUnavailable):
		return nil, zero, errors.New(
			"the selected payment method is not offered for this " +
				"delivery; submit another advertised instrument")
	case errors.Is(err, checkout.ErrPaymentMethodUnsupported):
		return nil, zero, errors.New(
			"this payment method needs the buyer to pay on the " +
				"store's checkout; use an offline method (e.g. cash " +
				"on delivery), Viva card payment, or hand over " +
				"get_checkout_link instead")
	default:
		return nil, zero, upstreamErr(err,
			"order placement failed; the cart is unchanged")
	}
}

// settleCompletion spends or releases a completion claim by what the
// attempt did about the order. It runs even when the call's context has
// ended (a platform that hung up, a shutdown): a retry must find what
// this attempt did, not a claim left in flight. The Django client's
// timeout bounds the one Redis write, which go-redis alone would retry
// against a silent Redis for several times that.
func (h *handlers) settleCompletion(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	idemKey string, placement checkout.Placement,
) {
	settle := h.deps.Checkout.MarkCompleted
	if placement == checkout.NothingPlaced {
		settle = h.deps.Checkout.ReleaseCompletion
	}
	settleCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), h.deps.Django.Timeout())
	defer cancel()
	if err := settle(settleCtx, t.SchemaName, s.ID, idemKey); err != nil {
		h.deps.Log.ErrorContext(ctx, "ucp: completion key not settled",
			slog.String("session", s.ID),
			slog.Bool("spent", placement != checkout.NothingPlaced),
			slog.String("error", err.Error()))
	}
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
		if errors.Is(err, checkout.ErrLocked) {
			return nil, nil, err
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
		if s.OrderUUID != "" {
			return nil, zero, unrenderedErr(s)
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

	if s.Status == checkout.StatusCanceled {
		return h.checkoutResult(ctx, t, s, neg)
	}
	// Once an order exists — completed, or escalated awaiting payment —
	// canceling the checkout would not cancel the order: the buyer could
	// still pay it, and the session would read canceled over a paid order.
	if s.Status == checkout.StatusCompleteInProgress {
		return nil, zero, checkout.ErrCompletionInProgress
	}
	if s.OrderUUID != "" {
		return nil, zero, fmt.Errorf(
			"checkout %s already placed order %s and cannot be canceled "+
				"here; the order is managed by the store",
			s.ID, s.OrderUUID)
	}
	s.Status = checkout.StatusCanceled
	if err := h.deps.Checkout.Save(ctx, s); err != nil {
		return nil, zero, errCheckoutUnavailable
	}
	return h.checkoutResult(ctx, t, s, neg)
}

// incompatibleCheckout is the negotiation-failure result: the platform
// shares no version of the checkout capability.
func incompatibleCheckout(links storefront.Links) CheckoutResult {
	return CheckoutResult{
		failure: ucp.CapabilitiesIncompatible(links.Cart()),
	}
}
