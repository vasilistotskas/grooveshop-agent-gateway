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
	"slices"
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

// gatewayCodes are the codes above: the ones the gateway mints.
var gatewayCodes = []string{
	CodeNotFound, CodeCartUnreachable, CodeUnauthorized, CodeNotGranted,
	CodeNotAllowed, CodeAccessOff, CodeThrottled, CodeConflict,
	CodeInvalid, CodeUnavailable,
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

// A call's outcome, beside the ToolError codes.
const (
	OutcomeOK    = "ok"
	OutcomeError = "error"
	OutcomePanic = "panic"
	// OutcomeRefused counts a ToolError whose code is the store's own
	// (Django's `code`, a refused coupon's reason): those are
	// open-ended, so the counter folds them into one value. The log line
	// keeps the code.
	OutcomeRefused = "refused"
)

// errPanicked answers a call whose handler panicked. The hint holds on
// every surface: it names neither the store's backend nor who asked.
var errPanicked = &ToolError{Code: CodeUnavailable,
	RetryAfter: UnavailableRetryAfter,
	Hint: "the tool failed unexpectedly; retry once, then try again " +
		"later"}

// Recorder is where a server records its tool calls: Server is one of
// the Server values, Metrics the registry gateway_tool_calls_total is
// counted on (nil counts nothing).
type Recorder struct {
	Server  string
	Log     *slog.Logger
	Metrics *obs.Metrics
}

// Add registers a tool on srv. Each call that reaches the handler logs
// one info line, "tool call", with KeyServer (server), the tool, the
// outcome (OutcomeOK, the ToolError's code, OutcomeError or
// OutcomePanic) and duration_ms; the logger's context handler adds the
// request id and the store. The arguments and the result are never
// logged: they carry customer data. The call is also counted on
// gateway_tool_calls_total by server, tool and outcome, a store's own
// code as OutcomeRefused. A call the SDK refuses before the handler, an
// unknown tool or arguments that fail the input schema, is neither
// logged nor counted.
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
//
// def's annotations are one of ReadOnly, Adds, Replaces or Changes;
// their title is the tool's Title too.
func Add[In, Out any](
	srv *mcp.Server, rec Recorder, def *mcp.Tool,
	h mcp.ToolHandlerFor[In, Out],
) {
	titled := *def
	titled.Title = def.Annotations.Title
	mcp.AddTool(srv, &titled, func(
		ctx context.Context, req *mcp.CallToolRequest, in In,
	) (*mcp.CallToolResult, Out, error) {
		ctx, stop := httpmw.UntilShutdown(ctx)
		defer stop()
		start := time.Now()
		res, out, err, panicked := run(ctx, rec.Log, def.Name, h, req, in)
		outcome := OutcomeOK
		switch {
		case panicked:
			outcome, err = OutcomePanic, errPanicked
		case err != nil:
			outcome = OutcomeError
			if te, ok := errors.AsType[*ToolError](err); ok {
				outcome = te.Code
			}
		}
		rec.Log.InfoContext(ctx, "tool call",
			slog.String(KeyServer, rec.Server),
			slog.String("tool", def.Name),
			slog.String("outcome", outcome),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()))
		if rec.Metrics != nil {
			rec.Metrics.ToolCalls.WithLabelValues(
				rec.Server, def.Name, counted(outcome)).Inc()
		}
		return res, out, err
	})
}

// counted is the counter's value for an outcome: the gateway's own as
// they are, a store's code as OutcomeRefused.
func counted(outcome string) string {
	switch {
	case outcome == OutcomeOK, outcome == OutcomeError,
		outcome == OutcomePanic, slices.Contains(gatewayCodes, outcome):
		return outcome
	}
	return OutcomeRefused
}

// The effect a tool has on the store, as the hints of its annotations,
// with its title. None reaches past the store, so none is open-world. An
// absent hint means destructive and open-world, and a host may then ask
// the person before every call. Add also sets the tool's own Title from
// the annotations' one: a client on protocol 2025-03-26, which the
// streamable transport assumes for a request naming no version, knows
// only annotations.title.

// ReadOnly is a tool that reads the store and changes nothing.
func ReadOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true,
		DestructiveHint: new(false), IdempotentHint: true,
		OpenWorldHint: new(false)}
}

// Adds is a tool that adds to the store (a cart, a cart line, an alert)
// and adds again when called again.
func Adds(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: new(false),
		OpenWorldHint: new(false)}
}

// Replaces is a tool that adds to the store and, in doing so, may end
// what the store held (a checkout started on a cart cancels the open
// checkout there), and does both again when called again.
func Replaces(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: new(true),
		OpenWorldHint: new(false)}
}

// Changes is a tool that replaces or removes what the store holds (a
// quantity, a line, a coupon, a checkout's state, an order placed), and
// changes nothing more when called again with the same arguments.
func Changes(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: new(true),
		IdempotentHint: true, OpenWorldHint: new(false)}
}

// The cacheScope values: who may keep a list result.
const (
	CachePublic  = "public"
	CachePrivate = "private"
)

// Cacheable is a server's ServerOptions.SetCacheable: its tools/list
// and server/discover answers may be kept for ttl, in scope. Left to
// the SDK, ttlMs is 0, and a client treats every list as stale at once.
func Cacheable(
	ttl time.Duration, scope string,
) func(context.Context, mcp.Request, *mcp.Cacheable) {
	return func(_ context.Context, _ mcp.Request, c *mcp.Cacheable) {
		c.TTLMs = int(ttl.Milliseconds())
		c.CacheScope = scope
	}
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
