package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/server"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := obs.NewLogger(cfg.LogLevel, cfg.Env, version)
	metrics := obs.NewMetrics()

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return err
	}
	rdb := redis.NewClient(redisOpts)
	defer func() { _ = rdb.Close() }()

	dj := django.New(
		cfg.DjangoBaseURL, cfg.DjangoPublicHost, cfg.InternalSecret,
		cfg.UpstreamTimeout, log, metrics,
	)
	resolver := tenant.NewResolver(
		dj, rdb, cfg.TenantCacheTTL, cfg.NegativeCacheTTL, log, metrics,
	)

	// Signing keys are per tenant schema and load lazily at first use.
	keys := ucp.NewKeys(rdb)
	// The pod name: webhook deliveries are claimed per consumer, and a
	// consumer that disappears has its pending deliveries taken over.
	consumer, err := os.Hostname()
	if err != nil {
		return err
	}
	dispatcher := ucp.NewDispatcher(rdb, keys, log, consumer,
		cfg.AllowLocalWebhooks)
	profiles, err := ucp.NewProfileResolver(cfg.AllowLocalWebhooks)
	if err != nil {
		return err
	}

	handler := server.New(server.Deps{
		Cfg:        cfg,
		Log:        log,
		Metrics:    metrics,
		Redis:      rdb,
		Django:     dj,
		Resolver:   resolver,
		Version:    version,
		Keys:       keys,
		Dispatcher: dispatcher,
		Profiles:   profiles,
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE (chat) and streamable MCP responses are
		// long-lived; per-handler deadlines bound the rest.
	}

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	// Order-webhook delivery stops consuming at the signal, so its drain
	// runs alongside the HTTP shutdown inside the pod's grace period;
	// deliveries enqueued meanwhile wait in the stream for another pod.
	// It is waited for before Redis closes: the deferred Close must not
	// race its final acknowledgements.
	dispatched := make(chan struct{})
	go func() {
		defer close(dispatched)
		dispatcher.Run(ctx)
	}()
	defer func() {
		stop() // a listen failure returns without the signal
		<-dispatched
	}()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", slog.String("addr", cfg.ListenAddr))
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// The preStop sleep in the Deployment keeps Traefik draining; this
	// timeout bounds in-flight requests after that.
	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), 15*time.Second,
	)
	defer cancel()
	log.Info("shutting down")
	return srv.Shutdown(shutdownCtx)
}
