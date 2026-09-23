package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
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
	// CartID and WebhookURL are additive members. UCP models neither: a
	// canonical caller sends line_items and discovers webhooks from the
	// platform profile, which this business does not yet dereference.
	// Consumers ignore members they do not recognise, so carrying them
	// costs conformance nothing while keeping the cart tools usable as a
	// path into checkout.
	CartID     string `json:"cart_id,omitempty" jsonschema:"an existing cart from the cart tools; omit when sending line_items"`
	WebhookURL string `json:"webhook_url,omitempty" jsonschema:"platform endpoint for signed order lifecycle webhooks"`
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
) (*mcp.CallToolResult, ucp.Checkout, error) {
	var zero ucp.Checkout
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	if err := in.Meta.validate(false); err != nil {
		return nil, zero, err
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

	cartID := in.CartID
	if cartID == "" {
		c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, "")
		if err != nil {
			return nil, zero, upstreamErr(err,
				"the cart service is unavailable")
		}
		cartID = c.UUID
		if err := checkout.SyncCartLines(
			ctx, h.deps.Django, t, cartID, lines,
		); err != nil {
			return nil, zero, upstreamErr(err,
				"a requested product was not found")
		}
	}

	// Reject an unusable endpoint here rather than queueing deliveries
	// to it: this tool is reachable anonymously, and the dispatcher
	// POSTs to whatever is stored on every order transition.
	if err := ucp.ValidateWebhookURL(
		in.WebhookURL, h.deps.AllowLocalWebhooks,
	); err != nil {
		return nil, zero, fmt.Errorf(
			"webhookUrl rejected: %w", err)
	}

	s := checkout.NewSession(t.SchemaName, t.Domain, checkout.ProtocolUCP, cartID)
	s.WebhookURL = in.WebhookURL
	codes, hasCodes := in.Checkout.discountCodes()
	if hasCodes {
		if err := checkout.ApplyDiscountCodes(
			ctx, h.deps.Django, t, s, codes,
		); err != nil {
			return nil, zero, upstreamErr(err,
				"the discount code could not be applied; the cart is "+
					"unchanged")
		}
	}
	in.Checkout.applyTo(s)
	if err := h.applyPayment(ctx, t, s, &in.Checkout); err != nil {
		return nil, zero, err
	}
	s.Recompute()
	if err := h.deps.Checkout.Save(ctx, s); err != nil {
		return nil, zero, errors.New(
			"checkout is temporarily unavailable; retry shortly")
	}
	res, out, err := h.checkoutResult(ctx, t, s)
	return h.discountAnnotated(res, out, err, s, hasCodes)
}

// applyPayment honours a payment selection submitted before completion:
// it changes the totals through the method's fee. A hosted pay_way_id
// and an advertised instrument are mutually exclusive.
func (h *handlers) applyPayment(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
	in *UCPCheckoutIn,
) error {
	if err := in.applyHostedSelection(t, s); err != nil {
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
) (*mcp.CallToolResult, ucp.Checkout, error) {
	var zero ucp.Checkout
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	if err := in.Meta.validate(false); err != nil {
		return nil, zero, err
	}
	s, release, err := h.lockedSession(ctx, t, in.ID)
	if err != nil {
		return nil, zero, err
	}
	defer release()

	if s.Terminal() || s.Status == checkout.StatusRequiresEscalation ||
		s.Status == checkout.StatusCompleteInProgress {
		return nil, zero, fmt.Errorf(
			"checkout %s is %s and can no longer be updated",
			s.ID, s.Status)
	}
	// line_items is full-state on update: the submitted list replaces
	// the cart's lines. Ignoring it would complete the old order.
	if in.Checkout.LineItems != nil {
		lines, err := in.Checkout.lines()
		if err != nil {
			return nil, zero, err
		}
		if len(lines) == 0 {
			return nil, zero, errors.New(
				"checkout.line_items must not be empty; cancel the " +
					"checkout to abandon it")
		}
		if err := checkout.SyncCartLines(
			ctx, h.deps.Django, t, s.CartID, lines,
		); err != nil {
			return nil, zero, upstreamErr(err,
				"the line items could not be applied; the checkout is "+
					"unchanged")
		}
	}
	codes, hasCodes := in.Checkout.discountCodes()
	if hasCodes {
		if err := checkout.ApplyDiscountCodes(
			ctx, h.deps.Django, t, s, codes,
		); err != nil {
			return nil, zero, upstreamErr(err,
				"the discount code could not be applied; the checkout is "+
					"unchanged")
		}
	}
	in.Checkout.applyTo(s)
	if err := h.applyPayment(ctx, t, s, &in.Checkout); err != nil {
		return nil, zero, err
	}
	s.Recompute()
	if err := h.deps.Checkout.Save(ctx, s); err != nil {
		return nil, zero, errors.New(
			"checkout is temporarily unavailable; retry shortly")
	}
	res, out, err := h.checkoutResult(ctx, t, s)
	return h.discountAnnotated(res, out, err, s, hasCodes)
}

func (h *handlers) completeCheckout(
	ctx context.Context, _ *mcp.CallToolRequest, in CompleteCheckoutIn,
) (*mcp.CallToolResult, ucp.Checkout, error) {
	var zero ucp.Checkout
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	if err := in.Meta.validate(true); err != nil {
		return nil, zero, err
	}
	s, release, err := h.lockedSession(ctx, t, in.ID)
	if err != nil {
		return nil, zero, err
	}
	defer release()

	switch s.Status {
	case checkout.StatusCompleted:
		return h.checkoutResult(ctx, t, s)
	case checkout.StatusCompleteInProgress:
		return nil, zero, checkout.ErrCompletionInProgress
	case checkout.StatusRequiresEscalation:
		// The escalation resolves through the order event, never through
		// a second complete — except to retry a payment link that could
		// not be created, for an order that already exists.
		if _, err := h.deps.Flow.ResumePayment(ctx, t, s); err != nil {
			return nil, zero, paymentLinkErr(s)
		}
		if err := h.deps.Checkout.Save(ctx, s); err != nil {
			return nil, zero, errors.New(
				"checkout is temporarily unavailable; retry shortly")
		}
		return h.checkoutResult(ctx, t, s)
	}

	// The submitted instrument decides how this order settles. Resolving
	// it here — rather than trusting a pay-way set earlier — is what
	// makes the payment object authoritative, and it rejects an
	// instrument the store cannot honour instead of placing an order the
	// buyer has no way to pay for.
	if err := h.applyPayment(ctx, t, s, &in.Checkout); err != nil {
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
		return nil, zero, errors.New(
			"checkout is temporarily unavailable; retry shortly")
	}
	if !claimed {
		// A prior attempt finished; render the session's current state.
		return h.checkoutResult(ctx, t, s)
	}

	_, err = h.deps.Flow.Complete(ctx, t, s)
	if err != nil && s.OrderUUID != "" {
		// The order exists whatever failed after it: spend the key so a
		// retry re-renders instead of placing a second order.
		_ = h.deps.Checkout.MarkCompleted(ctx, t.SchemaName, s.ID, idemKey)
		_ = h.deps.Checkout.Save(ctx, s)
		return nil, zero, paymentLinkErr(s)
	}
	if err != nil {
		h.deps.Checkout.ReleaseCompletion(ctx, t.SchemaName, s.ID, idemKey)
		_ = h.deps.Checkout.Save(ctx, s)
		switch {
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

	// The order exists now, whatever the save or render below does: the
	// key is spent first so a retry re-renders, never places a second
	// order.
	_ = h.deps.Checkout.MarkCompleted(ctx, t.SchemaName, s.ID, idemKey)
	if err := h.deps.Checkout.Save(ctx, s); err != nil {
		return nil, zero, errors.New(
			"the order was placed but checkout state could not be saved; " +
				"track it with track_order using orderUuid " + s.OrderUUID)
	}
	return h.checkoutResult(ctx, t, s)
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
		return nil, nil, errors.New(
			"checkout is temporarily unavailable; retry shortly")
	}
	s, err := h.deps.Checkout.Load(ctx, t.SchemaName, id)
	if err != nil {
		release()
		if errors.Is(err, checkout.ErrNotFound) {
			return nil, nil, errors.New(
				"that checkout no longer exists; start a new one with " +
					"create_checkout")
		}
		return nil, nil, errors.New(
			"checkout is temporarily unavailable; retry shortly")
	}
	return s, release, nil
}

func (h *handlers) checkoutResult(
	ctx context.Context, t *tenant.Tenant, s *checkout.Session,
) (*mcp.CallToolResult, ucp.Checkout, error) {
	var zero ucp.Checkout
	payload, err := h.deps.UCP.BuildCheckout(ctx, t, s)
	if err != nil {
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
	return textResult("%s", text), *payload, nil
}

// discountAnnotated appends the outcome of a discount submission to the
// tool's summary text so the agent sees rejections without digging into
// the session state.
func (h *handlers) discountAnnotated(
	res *mcp.CallToolResult, out ucp.Checkout, err error,
	s *checkout.Session, submitted bool,
) (*mcp.CallToolResult, ucp.Checkout, error) {
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
) (*mcp.CallToolResult, ucp.Checkout, error) {
	var zero ucp.Checkout
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	if err := in.Meta.validate(false); err != nil {
		return nil, zero, err
	}
	if in.ID == "" {
		return nil, zero, errors.New("id is required")
	}
	s, err := h.deps.Checkout.Load(ctx, t.SchemaName, in.ID)
	if err != nil {
		if errors.Is(err, checkout.ErrNotFound) {
			return nil, zero, errors.New(
				"that checkout no longer exists; start a new one with " +
					"create_checkout")
		}
		return nil, zero, errors.New(
			"checkout is temporarily unavailable; retry shortly")
	}
	return h.checkoutResult(ctx, t, s)
}

// cancelCheckout abandons a session the buyer is no longer pursuing.
//
// A terminal session is re-rendered rather than refused: cancel is
// idempotent by nature, and a platform retrying after a lost response
// must not receive an error for work already done.
func (h *handlers) cancelCheckout(
	ctx context.Context, _ *mcp.CallToolRequest, in CancelCheckoutIn,
) (*mcp.CallToolResult, ucp.Checkout, error) {
	var zero ucp.Checkout
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	if err := in.Meta.validate(true); err != nil {
		return nil, zero, err
	}
	s, release, err := h.lockedSession(ctx, t, in.ID)
	if err != nil {
		return nil, zero, err
	}
	defer release()

	if s.Status == checkout.StatusCanceled {
		return h.checkoutResult(ctx, t, s)
	}
	// Once an order exists — completed, or escalated awaiting payment —
	// canceling the checkout would not cancel the order: the buyer could
	// still pay it, and the session would read canceled over a paid order.
	if s.OrderUUID != "" || s.Status == checkout.StatusCompleteInProgress {
		return nil, zero, fmt.Errorf(
			"checkout %s already placed order %s and cannot be canceled "+
				"here; the order is managed by the store",
			s.ID, s.OrderUUID)
	}
	s.Status = checkout.StatusCanceled
	if err := h.deps.Checkout.Save(ctx, s); err != nil {
		return nil, zero, errors.New(
			"checkout is temporarily unavailable; retry shortly")
	}
	return h.checkoutResult(ctx, t, s)
}
