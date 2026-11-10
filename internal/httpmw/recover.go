package httpmw

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover turns a handler panic into a 500 instead of tearing down the
// connection. A response already under way keeps the status it sent;
// the panic is logged either way. http.ErrAbortHandler is re-raised:
// net/http aborts the response with it and logs nothing.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := wrap(w)
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if err, ok := rec.(error); ok &&
					errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				log.ErrorContext(r.Context(), "panic recovered",
					slog.Any("panic", rec),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)
				if !sw.started() {
					http.Error(sw, "internal server error",
						http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}
