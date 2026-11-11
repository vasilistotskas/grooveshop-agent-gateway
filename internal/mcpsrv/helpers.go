package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcptool"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/media"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/text"
)

// handlers hosts every tool implementation. The same methods back the MCP
// server and the first-party chatbot loop.
type handlers struct {
	deps Deps
	// surface is who the server the handlers serve is built for.
	surface surface
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

// withHint adds a sentence to the hint of the ToolError err is, on a copy:
// what the failed call left behind, which the hint for its cause cannot
// know.
func withHint(err error, sentence string) error {
	te, ok := errors.AsType[*mcptool.ToolError](err)
	if !ok {
		return err
	}
	out := *te
	out.Hint += ". " + sentence
	return &out
}

// shopperCartUnreachable is the hint for a chat shopper's own cart the
// store cannot reach: their signed-in cart's token was refused, or their
// guest cart is gone. Reloading the page has the storefront fetch their
// cart again (a new one for a gone guest cart), and the widget sends that.
const shopperCartUnreachable = "the shopper's cart could not be " +
	"reached, so nothing in it changed; tell the shopper their cart " +
	"could not be reached and to reload the page and try again"

// mcpCartGone is the /mcp hint for a cart the store no longer serves: the
// agent owns its cart id and can start another.
const mcpCartGone = "that cart no longer exists; create a new one with " +
	"create_cart"

// cartErr is upstreamErr for a call that names a cart, whose 404 is
// notFound (a line, a product). In the chat the cart is the shopper's own
// browser cart, which a cart the model created would never replace (the
// widget adopts a cart id only when it sent none): one the store cannot
// reach is shopperCartUnreachable there, whatever notFound says.
func (h *handlers) cartErr(err error, notFound string) error {
	if h.surface == chatSurface &&
		errors.Is(err, django.ErrCartUnreachable) {
		return shopperCartErr()
	}
	return upstreamErr(err, notFound)
}

// cartGoneErr is cartErr for a call on the cart itself, whose 404 (and
// 403, which upstreamErr reads as one) is the cart gone:
// shopperCartUnreachable in the chat, which serves no create_cart, and
// mcpCartGone on /mcp.
func (h *handlers) cartGoneErr(err error) error {
	if h.surface == chatSurface && (errors.Is(err, django.ErrNotFound) ||
		errors.Is(err, django.ErrForbidden)) {
		return shopperCartErr()
	}
	return h.cartErr(err, mcpCartGone)
}

// shopperCartErr is the chat's answer for the shopper's cart the store
// cannot reach.
func shopperCartErr() *mcptool.ToolError {
	return &mcptool.ToolError{Code: mcptool.CodeCartUnreachable,
		Hint: shopperCartUnreachable}
}

// upstreamErr converts Django client errors into agent-actionable tool
// errors. The SDK packs returned errors into CallToolResult.IsError.
func upstreamErr(err error, notFound string) error {
	apiErr, _ := errors.AsType[*django.APIError](err)
	// storeSaid is the store's own code and explanation for a refusal.
	storeSaid := func(fallback, hint string) *mcptool.ToolError {
		e := &mcptool.ToolError{Code: fallback, Hint: hint}
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
		return shopperCartErr()
	case errors.Is(err, django.ErrNotFound),
		errors.Is(err, django.ErrCartUnreachable),
		// Guest-flow 403s (e.g. a wrong order UUID) are "not yours to
		// see" — for an agent that is indistinguishable from not found.
		errors.Is(err, django.ErrForbidden):
		return &mcptool.ToolError{Code: mcptool.CodeNotFound, Hint: notFound}
	case errors.Is(err, django.ErrUnauthorized):
		return &mcptool.ToolError{Code: mcptool.CodeUnauthorized,
			Hint: "authentication is required or the access token has " +
				"expired; re-link the shopper's account and retry"}
	case errors.Is(err, django.ErrThrottled):
		e := storeSaid(mcptool.CodeThrottled, "the store is rate limiting "+
			"requests; tell the shopper to wait, then retry")
		if apiErr != nil {
			e.RetryAfter = int(apiErr.RetryAfter.Seconds())
		}
		return e
	case errors.Is(err, django.ErrConflict):
		return storeSaid(mcptool.CodeConflict, "the store refused the change in "+
			"its current state; explain the detail to the shopper")
	case errors.Is(err, django.ErrValidation):
		return storeSaid(mcptool.CodeInvalid, "the store rejected the request; "+
			"explain the detail to the shopper, or correct the arguments")
	default:
		return &mcptool.ToolError{Code: mcptool.CodeUnavailable,
			RetryAfter: mcptool.UnavailableRetryAfter,
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
