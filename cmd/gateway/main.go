package main

import (
	"context"
	"log/slog"
	"net"
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

// bootCheckTimeout bounds the check, before serving, that the
// key-encryption key opens the stored signing keys.
const bootCheckTimeout = 30 * time.Second

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
	// A key-encryption key that opens none of the stored keys (another
	// environment's) would boot and then serve every profile as 503:
	// refuse to start instead, as config.Load refuses a malformed one.
	bootCtx, cancelBoot := context.WithTimeout(context.Background(),
		bootCheckTimeout)
	err = keys.CheckKEK(bootCtx, log)
	cancelBoot()
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
	// The signing keys this pod holds are re-read every few minutes, so
	// the shared Redis does not evict them as idle, and written back if
	// it did. Stopped before Redis closes, like the dispatcher.
	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		keys.KeepFresh(ctx, log)
	}()
	defer func() {
		stop() // a listen failure returns without the signal
		<-dispatched
		<-refreshed
	}()

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	return serve(ctx, ln, handler,
		podTiming(completionTail(dj.Timeout())), log)
}
