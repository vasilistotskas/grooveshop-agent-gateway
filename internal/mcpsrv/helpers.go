package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/media"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/text"
)

// handlers hosts every tool implementation. The same methods back the MCP
// server and the first-party chatbot loop.
type handlers struct {
	deps Deps
}

// tenantFor extracts the request tenant. The /mcp route sits behind the
// tenant middleware, so absence is an internal wiring bug, not user error.
func (h *handlers) tenantFor(ctx context.Context) (*tenant.Tenant, error) {
	t, ok := tenant.FromContext(ctx)
	if !ok {
		return nil, errors.New(
			"internal error: request is missing store context; retry")
	}
	return t, nil
}

// upstreamErr converts Django client errors into agent-actionable tool
// errors. The SDK packs returned errors into CallToolResult.IsError.
func upstreamErr(err error, notFound string) error {
	switch {
	case errors.Is(err, django.ErrNotFound):
		return errors.New(notFound)
	// Guest-flow 403s (e.g. a wrong order UUID) are "not yours to see" —
	// for an agent that is indistinguishable from not found.
	case errors.Is(err, django.ErrForbidden):
		return errors.New(notFound)
	case errors.Is(err, django.ErrUnauthorized):
		return errors.New(
			"authentication is required or the access token has expired; " +
				"re-link the shopper's account and retry")
	case errors.Is(err, django.ErrThrottled):
		return errors.New(
			"the store is rate limiting requests; wait a minute and retry")
	case errors.Is(err, django.ErrValidation):
		var apiErr *django.APIError
		if errors.As(err, &apiErr) && apiErr.Detail != "" {
			return fmt.Errorf("the store rejected the request: %s",
				apiErr.Detail)
		}
		return errors.New("the store rejected the request as invalid")
	default:
		return errors.New("the store's backend is temporarily " +
			"unavailable; retry in about 30 seconds")
	}
}

// textResult builds a CallToolResult carrying a short human-readable
// summary; the SDK adds the typed output as structuredContent.
func textResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf(format, args...)},
		},
	}
}

// truncate cuts s to at most max runes, backing up to the last word
// boundary when it does cut, and marks the cut with an ellipsis.
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	cut := text.Runes(s, max)
	if len(cut) == len(s) {
		return s
	}
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i]
	}
	return cut + text.Ellipsis
}

func (h *handlers) imageURL(t *tenant.Tenant, path string) string {
	host := media.Host(t.AssetsDomain, h.deps.AssetsHost)
	return media.ImageURL(h.deps.MediaURLTemplate, host, t.SchemaName, path)
}

// num renders a json.Number for structured output, defaulting empty values
// to "0".
func num(n json.Number) string {
	if n == "" {
		return "0"
	}
	return n.String()
}

// posNum renders a json.Number only when it is a positive amount — used
// for discount fields that structured outputs omit at zero.
func posNum(n json.Number) string {
	if n == "" {
		return ""
	}
	f, err := n.Float64()
	if err != nil || f <= 0 {
		return ""
	}
	return n.String()
}
