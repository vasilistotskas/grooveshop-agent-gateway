package django

import (
	"context"
	"encoding/json"
)

// The agent surface (/api/v1/agent/*) accepts ONLY allauth.idp OIDC
// bearer tokens — the gateway forwards the agent's token verbatim and
// Django enforces validity and scopes. 401 unwraps to ErrUnauthorized
// (bad/expired token), 403 to ErrForbidden (missing scope).

// AgentProfile is the linked shopper's identity (scope: profile).
type AgentProfile struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
}

// AgentOrder is one row of the linked shopper's recent orders
// (OrderSerializer subset; scope: orders:read).
type AgentOrder struct {
	ID                   int64       `json:"id"`
	UUID                 string      `json:"uuid"`
	Status               string      `json:"status"`
	StatusDisplay        string      `json:"statusDisplay"`
	PaymentStatus        string      `json:"paymentStatus"`
	PaymentStatusDisplay string      `json:"paymentStatusDisplay"`
	IsPaid               bool        `json:"isPaid"`
	CreatedAt            string      `json:"createdAt"`
	TotalPriceItems      json.Number `json:"totalPriceItems"`
	TotalPriceExtra      json.Number `json:"totalPriceExtra"`
}

// AgentLoyalty is the linked shopper's loyalty summary (scope:
// loyalty:read).
type AgentLoyalty struct {
	PointsBalance    json.Number `json:"pointsBalance"`
	TotalXP          json.Number `json:"totalXp"`
	Level            json.Number `json:"level"`
	PointsToNextTier json.Number `json:"pointsToNextTier"`
	Tier             *struct {
		Translations map[string]struct {
			Name string `json:"name"`
		} `json:"translations"`
	} `json:"tier"`
}

func bearerHeaders(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func (c *Client) AgentMe(
	ctx context.Context, host, lang, token string,
) (*AgentProfile, error) {
	var out AgentProfile
	err := c.get(ctx, request{
		path:     "/agent/me",
		host:     host,
		language: lang,
		headers:  bearerHeaders(token),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) AgentOrders(
	ctx context.Context, host, lang, token string,
) ([]AgentOrder, error) {
	var out []AgentOrder
	err := c.get(ctx, request{
		path:     "/agent/me/orders",
		host:     host,
		language: lang,
		headers:  bearerHeaders(token),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) AgentLoyalty(
	ctx context.Context, host, lang, token string,
) (*AgentLoyalty, error) {
	var out AgentLoyalty
	err := c.get(ctx, request{
		path:     "/agent/me/loyalty",
		host:     host,
		language: lang,
		headers:  bearerHeaders(token),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
