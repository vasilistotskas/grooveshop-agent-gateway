package obs

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
)

// Recover, deferred in a function with a named error result, turns a
// panic into that error. The panic and its stack are logged; the error
// names op only, because it may reach a client (an MCP tool result).
//
// singleflight needs it: a panic in a DoChan closure is re-raised on a
// goroutine of its own, out of reach of every recover, and ends the
// process.
func Recover(ctx context.Context, log *slog.Logger, op string, err *error) {
	if p := recover(); p != nil {
		log.ErrorContext(ctx, "panic recovered",
			slog.String("op", op),
			slog.Any("panic", p),
			slog.String("stack", string(debug.Stack())),
		)
		*err = errors.New(op + ": panicked")
	}
}
