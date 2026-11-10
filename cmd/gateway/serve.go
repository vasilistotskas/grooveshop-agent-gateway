package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
)

// timing is the server's request-read bound and its shutdown budget.
//
// Shutdown: Kubernetes runs the Deployment's preStop sleep (10s, while
// Traefik drops the pod) before SIGTERM reaches us, and kills the
// process terminationGracePeriodSeconds (30s) after the sleep began:
// drain + final (15s) must end before that, with room to close what
// remains and exit. Issue #117 revisits the drain: it will be derived
// from the order-placement budget, with the grace period raised.
type timing struct {
	// read bounds reading a whole request, body included; the server
	// clears it once the body is read, so streams outlive it.
	read time.Duration
	// drain lets in-flight requests finish with their contexts live: a
	// context cancelled at the signal would abort a Django call mid-way.
	drain time.Duration
	// final is what a request still open after the drain (a chat or MCP
	// stream) gets, once its context is cancelled with
	// httpmw.ErrShuttingDown, to write its last event and return.
	final time.Duration
}

var podTiming = timing{
	// Bodies here are capped at a few hundred KiB. Half of Traefik's
	// entrypoint readTimeout (60s), so ours is never the looser bound for
	// a proxied client, and in-cluster callers are bounded too.
	read:  30 * time.Second,
	drain: 10 * time.Second,
	final: 5 * time.Second,
}

const (
	readHeaderTimeout = 10 * time.Second
	// Above Traefik's backend idleConnTimeout (90s by default, not
	// overridden in the infrastructure repo), so Traefik closes an idle
	// connection first and never sends a request on one we are closing.
	idleTimeout = 120 * time.Second
)

// serve runs handler on ln until ctx is done, then shuts down within
// the drain and final windows. Requests that outlive them are cut and
// logged; that is a clean stop, not an error. Only a failure to serve is
// returned.
func serve(
	ctx context.Context, ln net.Listener, handler http.Handler,
	t timing, log *slog.Logger,
) error {
	base, endRequests := context.WithCancelCause(context.Background())
	defer endRequests(nil)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       t.read,
		IdleTimeout:       idleTimeout,
		BaseContext:       func(net.Listener) context.Context { return base },
		// net/http's own lines (a panic in the middleware outside
		// httpmw.Recover, an accept failure) go through the gateway's
		// handler, as JSON like every other line.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
		// No WriteTimeout: it is a deadline on every write of the
		// response, so it would cut SSE (chat) and streamable MCP.
	}

	served := make(chan error, 1)
	go func() {
		log.InfoContext(ctx, "listening",
			slog.String("addr", ln.Addr().String()))
		served <- srv.Serve(ln)
	}()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	log.InfoContext(ctx, "shutting down")
	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), t.drain+t.final,
	)
	defer cancel()
	cut := time.AfterFunc(t.drain, func() {
		endRequests(httpmw.ErrShuttingDown)
	})
	defer cut.Stop()
	err := srv.Shutdown(shutdownCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	log.WarnContext(ctx,
		"closing requests still open after the shutdown window",
		slog.Duration("window", t.drain+t.final))
	return srv.Close()
}
