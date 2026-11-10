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

// The pod's shutdown budget. Kubernetes runs the Deployment's preStop
// sleep (10s, while Traefik drops the pod) before SIGTERM reaches us,
// and kills the process terminationGracePeriodSeconds (30s) after the
// sleep began: the window below (15s) must end before that, with room to
// close what remains and exit.
type shutdownWindow struct {
	// drain lets in-flight requests finish with their contexts live: a
	// context cancelled at the signal would abort a Django call mid-way.
	drain time.Duration
	// final is what a request still open after the drain (a chat or MCP
	// stream) gets, once its context is cancelled with
	// httpmw.ErrShuttingDown, to write its last event and return.
	final time.Duration
}

var podShutdown = shutdownWindow{
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
// window. Requests that outlive the window are cut and logged; that is a
// clean stop, not an error. Only a failure to serve is returned.
func serve(
	ctx context.Context, ln net.Listener, handler http.Handler,
	window shutdownWindow, log *slog.Logger,
) error {
	base, endRequests := context.WithCancelCause(context.Background())
	defer endRequests(nil)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		BaseContext:       func(net.Listener) context.Context { return base },
		// No ReadTimeout or WriteTimeout: SSE (chat) and streamable MCP
		// responses are long-lived, and a read deadline that passes
		// while the server watches the connection for a client close
		// cancels the request. Bodies are size-capped per handler, and
		// Traefik's entrypoint readTimeout bounds reading them.
	}

	served := make(chan error, 1)
	go func() {
		log.Info("listening", slog.String("addr", ln.Addr().String()))
		served <- srv.Serve(ln)
	}()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), window.drain+window.final,
	)
	defer cancel()
	cut := time.AfterFunc(window.drain, func() {
		endRequests(httpmw.ErrShuttingDown)
	})
	defer cut.Stop()
	err := srv.Shutdown(shutdownCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	log.Warn("closing requests still open after the shutdown window",
		slog.Duration("window", window.drain+window.final))
	return srv.Close()
}
