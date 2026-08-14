package mcpsrv

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

type SubscribeAlertIn struct {
	ProductID   int64  `json:"productId"`
	Kind        string `json:"kind" jsonschema:"restock (notify when back in stock) or price_drop (notify when the price falls to targetPrice)"`
	Email       string `json:"email" jsonschema:"where the notification is sent"`
	TargetPrice string `json:"targetPrice,omitempty" jsonschema:"required for price_drop; must be at or below the current price"`
}

type SubscribeAlertOut struct {
	Subscribed        bool   `json:"subscribed"`
	AlreadySubscribed bool   `json:"alreadySubscribed"`
	Kind              string `json:"kind"`
	ProductID         int64  `json:"productId"`
}

func (h *handlers) subscribeProductAlert(
	ctx context.Context, _ *mcp.CallToolRequest, in SubscribeAlertIn,
) (*mcp.CallToolResult, SubscribeAlertOut, error) {
	var out SubscribeAlertOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	if in.ProductID <= 0 || in.Email == "" {
		return nil, out, errors.New("productId and email are required")
	}
	if in.Kind != "restock" && in.Kind != "price_drop" {
		return nil, out, errors.New(
			`kind must be "restock" or "price_drop"`)
	}
	if in.Kind == "price_drop" && in.TargetPrice == "" {
		return nil, out, errors.New(
			"targetPrice is required for price_drop alerts")
	}

	_, err = h.deps.Django.CreateProductAlert(ctx, t.Domain, t.DefaultLocale,
		in.Kind, in.ProductID, in.Email, in.TargetPrice)
	out.Kind = in.Kind
	out.ProductID = in.ProductID
	switch {
	case err == nil:
		out.Subscribed = true
	case errors.Is(err, django.ErrConflict):
		// A duplicate subscription is a satisfied outcome, not a failure.
		out.Subscribed = true
		out.AlreadySubscribed = true
	default:
		return nil, out, upstreamErr(err, fmt.Sprintf(
			"product %d was not found", in.ProductID))
	}

	msg := "Done — %s will be emailed when product %d "
	if in.Kind == "restock" {
		msg += "is back in stock."
	} else {
		msg += fmt.Sprintf("drops to %s.", in.TargetPrice)
	}
	if out.AlreadySubscribed {
		msg = "That email is already subscribed to this alert for " +
			"product %d — nothing more to do. (%s)"
		return textResult(msg, in.ProductID, in.Email), out, nil
	}
	return textResult(msg, in.Email, in.ProductID), out, nil
}
