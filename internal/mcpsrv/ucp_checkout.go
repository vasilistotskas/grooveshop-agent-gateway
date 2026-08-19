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

type UCPLineItemIn struct {
	ProductID int64 `json:"productId"`
	Quantity  int   `json:"quantity" jsonschema:"default 1"`
}

type UCPBuyerIn struct {
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
}

type CreateCheckoutIn struct {
	CartID      string                `json:"cartId,omitempty" jsonschema:"an existing cart from the cart tools; omit when passing lineItems"`
	LineItems   []UCPLineItemIn       `json:"lineItems,omitempty" jsonschema:"products to buy; a cart is created for them"`
	Buyer       *UCPBuyerIn           `json:"buyer,omitempty"`
	Fulfillment *checkout.Fulfillment `json:"fulfillment,omitempty" jsonschema:"delivery details; kind home_delivery or pickup_point with providerCode acs/boxnow and the pickup ids from find_pickup_points"`
	PayWayID    int64                 `json:"payWayId,omitempty" jsonschema:"payment method id from get_payment_methods"`
	WebhookURL  string                `json:"webhookUrl,omitempty" jsonschema:"platform endpoint for signed order lifecycle webhooks"`
}

type UpdateCheckoutIn struct {
	CheckoutID  string                `json:"checkoutId"`
	Buyer       *UCPBuyerIn           `json:"buyer,omitempty"`
	Fulfillment *checkout.Fulfillment `json:"fulfillment,omitempty"`
	PayWayID    int64                 `json:"payWayId,omitempty"`
}

type CompleteCheckoutIn struct {
	CheckoutID     string `json:"checkoutId"`
	IdempotencyKey string `json:"idempotencyKey,omitempty" jsonschema:"repeat with the same key to retry safely"`
}

func (h *handlers) createCheckout(
	ctx context.Context, _ *mcp.CallToolRequest, in CreateCheckoutIn,
) (*mcp.CallToolResult, ucp.Checkout, error) {
	var zero ucp.Checkout
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	if in.CartID == "" && len(in.LineItems) == 0 {
		return nil, zero, errors.New(
			"provide cartId or lineItems to start a checkout")
	}

	cartID := in.CartID
	if cartID == "" {
		c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, "")
		if err != nil {
			return nil, zero, upstreamErr(err,
				"the cart service is unavailable")
		}
		cartID = c.UUID
		for _, li := range in.LineItems {
			qty := li.Quantity
			if qty <= 0 {
				qty = 1
			}
			if _, err := h.deps.Django.AddCartItem(
				ctx, t.Domain, t.DefaultLocale, cartID, li.ProductID, qty,
			); err != nil {
				return nil, zero, upstreamErr(err, fmt.Sprintf(
					"product %d was not found", li.ProductID))
			}
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

	s := checkout.NewSession(t.SchemaName, t.Domain, "ucp", cartID)
	s.WebhookURL = in.WebhookURL
	applyCheckoutInputs(s, in.Buyer, in.Fulfillment, in.PayWayID)
	s.Recompute()
	if err := h.deps.Checkout.Save(ctx, s); err != nil {
		return nil, zero, errors.New(
			"checkout is temporarily unavailable; retry shortly")
	}
	return h.checkoutResult(ctx, t, s)
}

func (h *handlers) updateCheckout(
	ctx context.Context, _ *mcp.CallToolRequest, in UpdateCheckoutIn,
) (*mcp.CallToolResult, ucp.Checkout, error) {
	var zero ucp.Checkout
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	s, release, err := h.lockedSession(ctx, t, in.CheckoutID)
	if err != nil {
		return nil, zero, err
	}
	defer release()

	if s.Terminal() || s.Status == checkout.StatusRequiresEscalation {
		return nil, zero, fmt.Errorf(
			"checkout %s is %s and can no longer be updated",
			s.ID, s.Status)
	}
	applyCheckoutInputs(s, in.Buyer, in.Fulfillment, in.PayWayID)
	s.Recompute()
	if err := h.deps.Checkout.Save(ctx, s); err != nil {
		return nil, zero, errors.New(
			"checkout is temporarily unavailable; retry shortly")
	}
	return h.checkoutResult(ctx, t, s)
}

func (h *handlers) completeCheckout(
	ctx context.Context, _ *mcp.CallToolRequest, in CompleteCheckoutIn,
) (*mcp.CallToolResult, ucp.Checkout, error) {
	var zero ucp.Checkout
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, zero, err
	}
	s, release, err := h.lockedSession(ctx, t, in.CheckoutID)
	if err != nil {
		return nil, zero, err
	}
	defer release()

	// A completed session re-renders its final state (idempotent reads).
	if s.Status == checkout.StatusCompleted {
		return h.checkoutResult(ctx, t, s)
	}
	if s.Status == checkout.StatusRequiresEscalation {
		return h.checkoutResult(ctx, t, s)
	}

	idemKey := in.IdempotencyKey
	if idemKey == "" {
		idemKey = "default"
	}
	_, claimed, err := h.deps.Checkout.ClaimCompletion(
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
	if err != nil {
		h.deps.Checkout.ReleaseCompletion(ctx, t.SchemaName, s.ID, idemKey)
		_ = h.deps.Checkout.Save(ctx, s)
		var shortfall *django.StockShortfall
		switch {
		case errors.As(err, &shortfall):
			var lines []string
			for _, item := range shortfall.FailedItems {
				lines = append(lines, fmt.Sprintf(
					"%s (product %d): requested %d, only %d available",
					item.ProductName, item.ProductID,
					item.Requested, item.Available))
			}
			return nil, zero, fmt.Errorf(
				"not enough stock: %s — adjust quantities and retry",
				strings.Join(lines, "; "))
		case errors.Is(err, checkout.ErrNotReady):
			return nil, zero, err
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

	if err := h.deps.Checkout.Save(ctx, s); err != nil {
		return nil, zero, errors.New(
			"the order was placed but checkout state could not be saved; " +
				"track it with track_order using orderUuid " + s.OrderUUID)
	}
	res, out, err := h.checkoutResult(ctx, t, s)
	if err == nil {
		if raw, mErr := marshalCheckout(out); mErr == nil {
			_ = h.deps.Checkout.StoreCompletion(
				ctx, t.SchemaName, s.ID, idemKey, raw)
		}
	}
	return res, out, err
}

func applyCheckoutInputs(
	s *checkout.Session, buyer *UCPBuyerIn,
	fulfillment *checkout.Fulfillment, payWayID int64,
) {
	if buyer != nil {
		s.Buyer = checkout.Buyer{
			FirstName: buyer.FirstName,
			LastName:  buyer.LastName,
			Email:     buyer.Email,
			Phone:     buyer.Phone,
		}
	}
	if fulfillment != nil {
		s.Fulfillment = *fulfillment
	}
	if payWayID > 0 {
		s.PayWayID = payWayID
	}
}

func (h *handlers) lockedSession(
	ctx context.Context, t *tenant.Tenant, id string,
) (*checkout.Session, func(), error) {
	if id == "" {
		return nil, nil, errors.New("checkoutId is required")
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

func marshalCheckout(c ucp.Checkout) ([]byte, error) {
	return jsonMarshal(c)
}
