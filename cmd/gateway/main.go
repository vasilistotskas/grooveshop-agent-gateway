package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

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
	cmd := run
	if len(os.Args) > 1 && os.Args[1] == ucp.SealCommand {
		cmd = sealSigningKeys
	}
	if err := cmd(); err != nil {
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

	redisOpts, err := config.RedisOptions(cfg.RedisURL)
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

	// Signing keys are per tenant schema and load lazily at first use,
	// sealing a key an earlier release stored unsealed.
	keys, err := ucp.NewKeys(rdb, cfg.SigningKeyKEK)
	if err != nil {
		return err
	}
	// The pod name: webhook deliveries are claimed per consumer, and a
	// consumer that disappears has its pending deliveries taken over.
	consumer, err := os.Hostname()
	if err != nil {
		return err
	}
	dispatcher := ucp.NewDispatcher(rdb, keys, log, consumer,
		cfg.AllowLocalWebhooks)
	profiles, err := ucp.NewProfileResolver(cfg.AllowLocalWebhooks, log)
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

	signaled, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()
	// The process also ends, with its cause, on a startup check's
	// definite failure.
	ctx, abort := context.WithCancelCause(signaled)
	defer abort(nil)

	// The key-encryption key is checked against the stored signing keys
	// while the server already answers: /healthz must, and /readyz
	// reports the pod not ready until the check passed, so the Service
	// sends it nothing else. The check waits for Redis as readiness does
	// (a pod booting while Redis restarts serves once it answers, never
	// crash-loops into the restart backoff), and ends the process only
	// on the definite failure: stored keys, none of which opens.
	//
	// Order-webhook delivery and the keys' refresh start once it passed.
	// Delivery stops consuming at the signal, so its drain runs alongside
	// the HTTP shutdown inside the pod's grace period; deliveries
	// enqueued meanwhile wait in the stream for another pod. Both are
	// waited for before Redis closes: the deferred Close must not race
	// the final acknowledgements.
	var background sync.WaitGroup
	background.Go(func() {
		if err := keys.AwaitKEK(ctx, log); err != nil {
			if errors.Is(err, ucp.ErrKEKOpensNoKey) {
				abort(err)
			}
			return
		}
		background.Go(func() { dispatcher.Run(ctx) })
		// The signing keys this pod holds are re-read every few minutes,
		// so the shared Redis does not evict them as idle, and written
		// back if it did.
		background.Go(func() { keys.KeepFresh(ctx, log) })
	})
	defer func() {
		abort(nil) // a listen failure returns without the signal
		background.Wait()
	}()

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	err = serve(ctx, ln, handler,
		podTiming(completionTail(dj.Timeout())), log)
	if cause := context.Cause(ctx); errors.Is(cause, ucp.ErrKEKOpensNoKey) {
		return cause
	}
	return err
}
