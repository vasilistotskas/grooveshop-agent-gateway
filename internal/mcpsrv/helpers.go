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
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
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

// linksFor are the store's page links in the language ctx is served in:
// a chat turn's, else the store's default (django.Language).
func (h *handlers) linksFor(
	ctx context.Context, t *tenant.Tenant,
) storefront.Links {
	return h.deps.Site.Links(t.Domain, django.Language(ctx, t.TenantConfig))
}

// ToolError is a failed tool call's answer, rendered as JSON text so an
// agent reads why the call failed and what to do, not just that it did:
//
//   - Code: the store's stable error code when it gave one for a refusal
//     the agent may explain (Django's `code`), else the gateway's own.
//   - Detail: the store's explanation, in the store's language.
//   - RetryAfter: seconds to wait before retrying, when known.
//   - Hint: what the agent should do next.
//
// Only what Django wrote for a client reaches it: never a raw body, a
// status line or a 5xx's text.
type ToolError struct {
	Code       string `json:"code"`
	Detail     string `json:"detail,omitempty"`
	RetryAfter int    `json:"retryAfterSeconds,omitempty"`
	Hint       string `json:"hint"`
}

func (e *ToolError) Error() string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(e)
	return strings.TrimSpace(b.String())
}

// Gateway codes for failures Django gave no code for, or whose code
// would say more than the agent may know (a 403 is reported as
// not_found).
const (
	codeNotFound        = "not_found"
	codeCartUnreachable = "cart_unreachable"
	codeUnauthorized    = "unauthorized"
	codeThrottled       = "throttled"
	codeConflict        = "conflict"
	codeInvalid         = "invalid"
	codeUnavailable     = "unavailable"
)

// upstreamErr converts Django client errors into agent-actionable tool
// errors. The SDK packs returned errors into CallToolResult.IsError.
func upstreamErr(err error, notFound string) error {
	apiErr, _ := errors.AsType[*django.APIError](err)
	// storeSaid is the store's own code and explanation for a refusal.
	storeSaid := func(fallback, hint string) *ToolError {
		e := &ToolError{Code: fallback, Hint: hint}
		if apiErr != nil {
			if apiErr.Code != "" {
				e.Code = apiErr.Code
			}
			e.Detail = apiErr.Detail
		}
		return e
	}
	switch {
	// Checked first: it is ErrCartUnreachable too, whose not-found
	// wording ("create a new cart") would mislead for the shopper's own.
	case errors.Is(err, django.ErrCartTokenRefused):
		return &ToolError{Code: codeCartUnreachable, Hint: "the shopper's " +
			"cart could not be reached, so nothing in it changed; tell " +
			"the shopper their cart could not be reached and to try again"}
	case errors.Is(err, django.ErrNotFound),
		errors.Is(err, django.ErrCartUnreachable),
		// Guest-flow 403s (e.g. a wrong order UUID) are "not yours to
		// see" — for an agent that is indistinguishable from not found.
		errors.Is(err, django.ErrForbidden):
		return &ToolError{Code: codeNotFound, Hint: notFound}
	case errors.Is(err, django.ErrUnauthorized):
		return &ToolError{Code: codeUnauthorized, Hint: "authentication " +
			"is required or the access token has expired; re-link the " +
			"shopper's account and retry"}
	case errors.Is(err, django.ErrThrottled):
		e := storeSaid(codeThrottled, "the store is rate limiting "+
			"requests; tell the shopper to wait, then retry")
		if apiErr != nil {
			e.RetryAfter = int(apiErr.RetryAfter.Seconds())
		}
		return e
	case errors.Is(err, django.ErrConflict):
		return storeSaid(codeConflict, "the store refused the change in "+
			"its current state; explain the detail to the shopper")
	case errors.Is(err, django.ErrValidation):
		return storeSaid(codeInvalid, "the store rejected the request; "+
			"explain the detail to the shopper, or correct the arguments")
	default:
		return &ToolError{Code: codeUnavailable, RetryAfter: 30,
			Hint: "the store's backend is temporarily unavailable; retry " +
				"once, then tell the shopper to try again later"}
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
