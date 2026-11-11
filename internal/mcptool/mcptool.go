// Package mcptool holds what the tools of both MCP servers, the shoppers'
// /mcp (internal/mcpsrv) and the store team's /mcp/admin
// (internal/adminmcp), answer with: a failed call's ToolError and the
// gateway's codes for it, and the marker on text a customer wrote. How
// each server maps a Django failure to a code is its own policy and stays
// in its package.
package mcptool

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolError is a failed tool call's answer, rendered as JSON text so an
// agent reads why the call failed and what to do, not just that it did:
//
//   - Code: the store's stable code when it gave one for a refusal the
//     agent may explain (Django's `code`, or a refused coupon's
//     `reason`), else the gateway's own.
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

// The gateway's codes, for failures Django gave no code for, or whose
// code would say more than the agent may know.
const (
	CodeNotFound        = "not_found"
	CodeCartUnreachable = "cart_unreachable"
	CodeUnauthorized    = "unauthorized"
	CodeNotGranted      = "not_granted"
	CodeNotAllowed      = "not_allowed"
	CodeAccessOff       = "access_off"
	CodeThrottled       = "throttled"
	CodeConflict        = "conflict"
	CodeInvalid         = "invalid"
	CodeUnavailable     = "unavailable"
)

// UnavailableRetryAfter is the wait, in seconds, an unavailable answer
// names: the store's backend or its checkout records.
const UnavailableRetryAfter = 30

// Untrusted is text a customer wrote (a review, a contact message, an
// order note), marked so a model reads it as data and not as instructions.
type Untrusted struct {
	Untrusted bool   `json:"untrusted"`
	Source    string `json:"source"`
	Text      string `json:"text"`
}

// MarkUntrusted marks s, written by source, as Untrusted; nil when s is
// empty, so the field is left out.
func MarkUntrusted(source, s string) *Untrusted {
	if s == "" {
		return nil
	}
	return &Untrusted{Untrusted: true, Source: source, Text: s}
}

// Add registers a tool with a log line per call: the tool, the store, the
// outcome and how long it took. Never its arguments or result, which carry
// customer data.
func Add[In, Out any](
	srv *mcp.Server, log *slog.Logger, def *mcp.Tool,
	h mcp.ToolHandlerFor[In, Out],
) {
	mcp.AddTool(srv, def, func(
		ctx context.Context, req *mcp.CallToolRequest, in In,
	) (*mcp.CallToolResult, Out, error) {
		start := time.Now()
		res, out, err := h(ctx, req, in)
		outcome := "ok"
		if err != nil {
			outcome = "error"
			var te *ToolError
			if errors.As(err, &te) {
				outcome = te.Code
			}
		}
		log.InfoContext(ctx, "staff tool call",
			slog.String("tool", def.Name),
			slog.String("outcome", outcome),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()))
		return res, out, err
	})
}
