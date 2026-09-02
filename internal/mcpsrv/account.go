package mcpsrv

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
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
				"%s to discover the authorization server, authorize with "+
				"the shopper, then retry with the access token",
			storefront.OAuthResourceMetadata(t.Domain))
	}
	return l, nil
}

// scopedErr maps a failure on an account endpoint: Django answers 403
// when the linked token was granted without the scope that endpoint
// needs, which the agent fixes by re-authorizing, not by retrying.
func scopedErr(err error, scope, notFound string) error {
	if errors.Is(err, django.ErrForbidden) {
		return fmt.Errorf("the linked account's token is missing the %s "+
			"scope; re-authorize requesting it", scope)
	}
	return upstreamErr(err, notFound)
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
		return nil, out, scopedErr(err, "orders:read", "no orders found")
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
			"tracking link. Amounts are VAT-inclusive %s.",
		len(out.Orders), t.DefaultCurrency), out, nil
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
		return nil, out, scopedErr(err, "loyalty:read", "no loyalty data found")
	}

	out.PointsBalance = num(loyalty.PointsBalance)
	out.TotalXP = num(loyalty.TotalXP)
	out.Level = num(loyalty.Level)
	if loyalty.PointsToNextTier != "" {
		out.PointsToNextTier = loyalty.PointsToNextTier.String()
	}
	if loyalty.Tier != nil {
		out.Tier = django.Localized(loyalty.Tier.Translations, t.DefaultLocale).Name
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

type MyFavouritesOut struct {
	Favourites []MyFavourite `json:"favourites"`
}

type MyFavourite struct {
	ProductID  int64  `json:"productId"`
	Name       string `json:"name"`
	FinalPrice string `json:"finalPrice"`
	Currency   string `json:"currency"`
	InStock    bool   `json:"inStock"`
	AddedAt    string `json:"addedAt"`
}

func (h *handlers) myFavourites(
	ctx context.Context, _ *mcp.CallToolRequest, _ struct{},
) (*mcp.CallToolResult, MyFavouritesOut, error) {
	var out MyFavouritesOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	linked, err := h.linkedFor(ctx, t)
	if err != nil {
		return nil, out, err
	}

	favourites, err := h.deps.Django.AgentFavourites(
		ctx, t.Domain, t.DefaultLocale, linked.Bearer)
	if err != nil {
		return nil, out, scopedErr(err, "favourites:read", "no favourites found")
	}

	out.Favourites = make([]MyFavourite, 0, len(favourites))
	for _, f := range favourites {
		out.Favourites = append(out.Favourites, MyFavourite{
			ProductID:  f.ProductID,
			Name:       f.Name,
			FinalPrice: num(f.FinalPrice),
			Currency:   f.Currency,
			InStock:    f.InStock,
			AddedAt:    f.AddedAt,
		})
	}

	return textResult(
		"The linked account has %d favourite products. Use them to "+
			"personalise suggestions (get_product for details, "+
			"add_to_cart to buy); prices are VAT-inclusive.",
		len(out.Favourites)), out, nil
}
