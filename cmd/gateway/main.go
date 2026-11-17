package main

import (
	"context"
	"errors"
	"io"
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
	os.Exit(start(os.Stdout, os.Args[1:]))
}

// start runs the command args name and returns the process's exit code.
// The logger is built first, at info until the configuration names the
// level, so every line, a boot failure's included, is JSON through the
// one handler.
func start(stdout io.Writer, args []string) int {
	level := new(slog.LevelVar)
	log := obs.NewLogger(stdout, level, version)
	cmd := run
	if len(args) > 0 && args[0] == ucp.SealCommand {
		cmd = sealSigningKeys
	}
	if err := cmd(log, level); err != nil {
		log.Error("fatal", slog.String("error", err.Error()))
		return 1
	}
	return 0
}

// configure loads the configuration and sets the logger's level and
// environment from it.
func configure(
	boot *slog.Logger, level *slog.LevelVar,
) (config.Config, *slog.Logger, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, nil, err
	}
	level.Set(cfg.LogLevel)
	return cfg, boot.With(slog.String(obs.KeyEnv, cfg.Env)), nil
}

// newRedis is the gateway's one Redis client, built from REDIS_URL. It
// routes go-redis's own log lines through log: go-redis has one logger
// for the process (redis.SetLogger), set here with the client.
func newRedis(rawURL string, log *slog.Logger) (*redis.Client, error) {
	opts, err := config.RedisOptions(rawURL)
	if err != nil {
		return nil, err
	}
	redis.SetLogger(obs.RedisLogger{Log: log})
	return redis.NewClient(opts), nil
}

func run(boot *slog.Logger, level *slog.LevelVar) error {
	cfg, log, err := configure(boot, level)
	if err != nil {
		return err
	}
	metrics := obs.NewMetrics()

	rdb, err := newRedis(cfg.RedisURL, log)
	if err != nil {
		return err
	}
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
	// Order-webhook delivery starts once it passed, so nothing is signed
	// before the key is known to open. It stops consuming at the signal,
	// so its drain runs alongside the HTTP shutdown inside the pod's
	// grace period; deliveries enqueued meanwhile wait in the stream for
	// another pod. It is waited for before Redis closes: the deferred
	// Close must not race its final acknowledgements.
	var background sync.WaitGroup
	background.Go(func() {
		if err := keys.AwaitKEK(ctx, log); err != nil {
			if errors.Is(err, ucp.ErrKEKOpensNoKey) {
				abort(err)
			}
			return
		}
		background.Go(func() { dispatcher.Run(ctx) })
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
