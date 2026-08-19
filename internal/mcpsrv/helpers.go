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
)

// handlers hosts every tool implementation. The same methods back the MCP
// server and (from milestone 4 on) the first-party chatbot loop.
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

// localized picks the tenant's locale from a parler translations map,
// falling back to any available language.
func localized[T any](translations map[string]T, locale string) T {
	if v, ok := translations[locale]; ok {
		return v
	}
	for _, v := range translations {
		return v
	}
	var zero T
	return zero
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i]
	}
	return cut + "…"
}

func (h *handlers) productURL(t *tenant.Tenant, id int64, slug string) string {
	return fmt.Sprintf("https://%s/products/%d/%s", t.Domain, id, slug)
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

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
