package httpmw

import (
	"context"
	"errors"
)

// ErrShuttingDown is the cause (context.Cause) of a request context the
// server cancels at the end of its shutdown drain: a streaming handler
// tells it from a client that left, and ends its stream with a final
// event.
var ErrShuttingDown = errors.New("server shutting down")

type shutdownKey struct{}

// WithShutdown returns base carrying itself as the server's shutdown:
// the context every request derives from, which the server cancels with
// ErrShuttingDown at the end of its drain. A context derived from it
// whose cancellation was cut off (context.WithoutCancel, the contexts
// the MCP SDK runs a tool on for a client on a protocol before
// 2026-07-28) keeps the value, so UntilShutdown still reaches it.
func WithShutdown(base context.Context) context.Context {
	return context.WithValue(base, shutdownKey{}, base)
}

// UntilShutdown returns ctx, also cancelled with the shutdown's cause
// when the server that ctx's request came through cuts its requests;
// stop releases it. A context that came through no such server (a test,
// a call made in process) is returned as it is.
func UntilShutdown(ctx context.Context) (
	_ context.Context, stop func(),
) {
	shutdown, ok := ctx.Value(shutdownKey{}).(context.Context)
	if !ok {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	unhook := context.AfterFunc(shutdown, func() {
		cancel(context.Cause(shutdown))
	})
	return ctx, func() {
		unhook()
		cancel(nil)
	}
}
