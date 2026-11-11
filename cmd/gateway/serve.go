package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
)

// timing is the server's request-read bound and its shutdown budget.
//
// Shutdown: Kubernetes runs the Deployment's preStop sleep (10s, while
// Traefik drops the pod) before SIGTERM reaches us, and kills the
// process terminationGracePeriodSeconds after the sleep began. So the
// grace period must cover the sleep, drain + final, and a margin to
// close what remains and exit. With UPSTREAM_TIMEOUT=10s final is 40s
// and the grace period 65s (grooveshop-infrastructure,
// base/agent-gateway.yaml); raising the timeout raises both.
type timing struct {
	// read bounds reading a whole request, body included; the server
	// clears it once the body is read, so streams outlive it.
	read time.Duration
	// drain lets in-flight requests finish with their contexts live: a
	// context cancelled at the signal would abort a Django call mid-way.
	drain time.Duration
	// final is what a request still open after the drain gets, once its
	// context is cancelled with httpmw.ErrShuttingDown: a stream (chat,
	// MCP) writes its last event, and an order completion that went on
	// without its caller places the order and settles its key.
	final time.Duration
}

// streamFinal is what a stream needs to write its last event.
const streamFinal = 5 * time.Second

// podTiming is the timing of a pod whose order completions keep their
// request open for up to completion after its context ends.
func podTiming(completion time.Duration) timing {
	return timing{
		// Bodies here are capped at a few hundred KiB. Half of Traefik's
		// entrypoint readTimeout (60s), so ours is never the looser bound
		// for a proxied client, and in-cluster callers are bounded too.
		read:  30 * time.Second,
		drain: 10 * time.Second,
		final: max(streamFinal, completion),
	}
}

// completionTail is the longest an order completion keeps its request
// open once the request's context has ended: the part of
// checkout.Flow.Complete that runs without its caller, then one upstream
// timeout in which the caller settles its idempotency key by the
// placement. A completion can detach until the end of the drain, so the
// final window must hold all of it.
//
// The budget is read from the flow's own deadline for a session stored
// in progress, so it cannot drift from what Complete runs on.
func completionTail(dj *django.Client, log *slog.Logger) time.Duration {
	var stored checkout.Session
	budget := checkout.NewFlow(dj, nil, storefront.Site{}, log).
		CompletionDeadline(&stored).Sub(stored.UpdatedAt)
	return budget + dj.Timeout()
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

	log.InfoContext(ctx, "shutting down",
		slog.Duration("window", t.drain+t.final))
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
