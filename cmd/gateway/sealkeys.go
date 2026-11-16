package main

// The move of the stores' signing keys to sealed records
// (internal/ucp/keys_unsealed.go). Delete this file with that one.

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// errSealConflicts fails a seal that left a store's records alone.
var errSealConflicts = errors.New(
	"some stores' signing keys were left unsealed; see the log")

// sealSigningKeys is the ucp.SealCommand subcommand, `/gateway
// seal-signing-keys`, which an operator runs once no pod of a release
// before the sealed keys is left: with the pod's own configuration it
// seals any store's key still unsealed, deletes every unsealed record
// whose key is sealed, and exits.
func sealSigningKeys() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := obs.NewLogger(cfg.LogLevel, cfg.Env, version)
	redisOpts, err := config.RedisOptions(cfg.RedisURL)
	if err != nil {
		return err
	}
	rdb := redis.NewClient(redisOpts)
	defer func() { _ = rdb.Close() }()
	keys, err := ucp.NewKeys(rdb, cfg.SigningKeyKEK)
	if err != nil {
		return err
	}
	return sealUnsealedKeys(context.Background(), keys, true, log)
}

// sealUnsealedKeys runs keys.SealUnsealed and logs what it did. A store
// it left alone is logged at error and fails the deleting run. At boot
// nothing it reports stops the pod: a store whose key it could not seal
// is refused (ucp.ErrKeyUnsealed), never given a new key.
func sealUnsealedKeys(
	ctx context.Context, keys *ucp.Keys, deleteUnsealed bool,
	log *slog.Logger,
) error {
	report, err := keys.SealUnsealed(ctx, deleteUnsealed)
	if len(report.Sealed)+len(report.Deleted) > 0 || deleteUnsealed {
		log.InfoContext(ctx, "signing keys sealed",
			slog.String("sealed", strings.Join(report.Sealed, ",")),
			slog.String("already_sealed",
				strings.Join(report.AlreadySealed, ",")),
			slog.String("unsealed_deleted",
				strings.Join(report.Deleted, ",")))
	}
	for _, schema := range slices.Sorted(maps.Keys(report.Conflicts)) {
		log.ErrorContext(ctx, "signing key left unsealed",
			slog.String(obs.KeyTenant, schema),
			slog.String("reason", report.Conflicts[schema]))
	}
	if err != nil {
		return err
	}
	if deleteUnsealed && len(report.Conflicts) > 0 {
		return errSealConflicts
	}
	return nil
}
