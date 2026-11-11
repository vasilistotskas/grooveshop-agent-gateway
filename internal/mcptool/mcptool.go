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

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
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

// KeyServer is the log key naming which surface served a call, with one
// of the Server values: two of them carry a tool of one name.
const KeyServer = "server"

// The surfaces, as KeyServer names them.
const (
	ServerMCP   = "mcp"
	ServerChat  = "chat"
	ServerAdmin = "mcp_admin"
)

// OutcomePanic is the outcome of a call whose handler panicked.
const OutcomePanic = "panic"

// errPanicked answers a call whose handler panicked. The hint holds on
// every surface: it names neither the store's backend nor who asked.
var errPanicked = &ToolError{Code: CodeUnavailable,
	RetryAfter: UnavailableRetryAfter,
	Hint: "the tool failed unexpectedly; retry once, then try again " +
		"later"}

// Add registers a tool on srv. Each call that reaches the handler logs
// one info line, "tool call", with KeyServer (server), the tool, the
// outcome (ok, the ToolError's code, error, or OutcomePanic) and
// duration_ms; the logger's context handler adds the request id and the
// store. The arguments and the result are never logged: they carry
// customer data. A call the SDK refuses before the handler, an unknown
// tool or arguments that fail the input schema, logs no line.
//
// The SDK runs a handler on a goroutine of its own, which no recover
// covers, so a panic would end the process. It is recovered here
// (obs.Recover logs it with its stack) and answered as unavailable.
//
// The SDK ends a handler's context with its request only for a client on
// protocol 2026-07-28 or later. Every handler's context ends at the
// server's shutdown cut, with httpmw.ErrShuttingDown, whatever the
// protocol (httpmw.UntilShutdown): work a tool has not detached stops
// there, as an ACP request's does.
func Add[In, Out any](
	srv *mcp.Server, log *slog.Logger, server string, def *mcp.Tool,
	h mcp.ToolHandlerFor[In, Out],
) {
	mcp.AddTool(srv, def, func(
		ctx context.Context, req *mcp.CallToolRequest, in In,
	) (*mcp.CallToolResult, Out, error) {
		ctx, stop := httpmw.UntilShutdown(ctx)
		defer stop()
		start := time.Now()
		res, out, err, panicked := run(ctx, log, def.Name, h, req, in)
		outcome := "ok"
		switch {
		case panicked:
			outcome, err = OutcomePanic, errPanicked
		case err != nil:
			outcome = "error"
			if te, ok := errors.AsType[*ToolError](err); ok {
				outcome = te.Code
			}
		}
		log.InfoContext(ctx, "tool call",
			slog.String(KeyServer, server),
			slog.String("tool", def.Name),
			slog.String("outcome", outcome),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()))
		return res, out, err
	})
}

// run calls h, reporting whether it panicked; obs.Recover logs the panic.
func run[In, Out any](
	ctx context.Context, log *slog.Logger, tool string,
	h mcp.ToolHandlerFor[In, Out], req *mcp.CallToolRequest, in In,
) (res *mcp.CallToolResult, out Out, err error, panicked bool) {
	defer obs.Recover(ctx, log, "mcp tool "+tool, &err)
	panicked = true
	res, out, err = h(ctx, req, in)
	return res, out, err, false
}
