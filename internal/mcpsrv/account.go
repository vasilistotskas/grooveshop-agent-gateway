package mcpsrv

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// linkedFor extracts the request's linked-account credential. The
// identity middleware only sets it for verified bearer tokens, so
// absence means the agent has not completed the OAuth account link.
func (h *handlers) linkedFor(
	ctx context.Context, t *tenant.Tenant,
) (*identity.Linked, error) {
	l, ok := identity.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf(
			"no linked account: complete the OAuth flow first — fetch "+
				"https://%s/.well-known/oauth-protected-resource/mcp to "+
				"discover the authorization server, authorize with the "+
				"shopper, then retry with the access token", t.Domain)
	}
	return l, nil
}

type MyOrdersOut struct {
	Orders []MyOrder `json:"orders"`
}

type MyOrder struct {
	ID            int64  `json:"id"`
	OrderUUID     string `json:"orderUuid"`
	Status        string `json:"status"`
	StatusDisplay string `json:"statusDisplay"`
	PaymentStatus string `json:"paymentStatus"`
	IsPaid        bool   `json:"isPaid"`
	PlacedAt      string `json:"placedAt"`
	ItemsTotal    string `json:"itemsTotal"`
	ExtrasTotal   string `json:"extrasTotal"`
}

func (h *handlers) myOrders(
	ctx context.Context, _ *mcp.CallToolRequest, _ struct{},
) (*mcp.CallToolResult, MyOrdersOut, error) {
	var out MyOrdersOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	linked, err := h.linkedFor(ctx, t)
	if err != nil {
		return nil, out, err
	}

	orders, err := h.deps.Django.AgentOrders(
		ctx, t.Domain, t.DefaultLocale, linked.Bearer)
	if err != nil {
		if errors.Is(err, django.ErrForbidden) {
			return nil, out, errors.New(
				"the linked account's token is missing the orders:read " +
					"scope; re-authorize requesting it")
		}
		return nil, out, upstreamErr(err, "no orders found")
	}

	out.Orders = make([]MyOrder, 0, len(orders))
	for _, o := range orders {
		out.Orders = append(out.Orders, MyOrder{
			ID:            o.ID,
			OrderUUID:     o.UUID,
			Status:        o.Status,
			StatusDisplay: o.StatusDisplay,
			PaymentStatus: o.PaymentStatusDisplay,
			IsPaid:        o.IsPaid,
			PlacedAt:      o.CreatedAt,
			ItemsTotal:    num(o.TotalPriceItems),
			ExtrasTotal:   num(o.TotalPriceExtra),
		})
	}

	return textResult(
		"Found %d recent orders for the linked account. Use track_order "+
			"with an orderUuid for fulfilment details and the carrier "+
			"tracking link. Amounts are VAT-inclusive EUR.",
		len(out.Orders)), out, nil
}

type MyLoyaltyOut struct {
	PointsBalance    string `json:"pointsBalance"`
	TotalXP          string `json:"totalXp"`
	Level            string `json:"level"`
	Tier             string `json:"tier,omitempty"`
	PointsToNextTier string `json:"pointsToNextTier,omitempty"`
}

func (h *handlers) myLoyaltyPoints(
	ctx context.Context, _ *mcp.CallToolRequest, _ struct{},
) (*mcp.CallToolResult, MyLoyaltyOut, error) {
	var out MyLoyaltyOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	linked, err := h.linkedFor(ctx, t)
	if err != nil {
		return nil, out, err
	}

	loyalty, err := h.deps.Django.AgentLoyalty(
		ctx, t.Domain, t.DefaultLocale, linked.Bearer)
	if err != nil {
		if errors.Is(err, django.ErrForbidden) {
			return nil, out, errors.New(
				"the linked account's token is missing the loyalty:read " +
					"scope; re-authorize requesting it")
		}
		return nil, out, upstreamErr(err, "no loyalty data found")
	}

	out.PointsBalance = num(loyalty.PointsBalance)
	out.TotalXP = num(loyalty.TotalXP)
	out.Level = num(loyalty.Level)
	if loyalty.PointsToNextTier != "" {
		out.PointsToNextTier = loyalty.PointsToNextTier.String()
	}
	if loyalty.Tier != nil {
		out.Tier = localized(loyalty.Tier.Translations, t.DefaultLocale).Name
	}

	summary := fmt.Sprintf(
		"The linked account has %s spendable loyalty points (level %s",
		out.PointsBalance, out.Level)
	if out.Tier != "" {
		summary += ", tier " + out.Tier
	}
	summary += ")."
	return textResult("%s", summary), out, nil
}
