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

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// errSealConflicts fails a seal that left a store's records alone.
var errSealConflicts = errors.New(
	"some stores' signing keys were left unsealed; see the log")

// sealSigningKeys is the ucp.SealCommand subcommand, `/gateway
// seal-signing-keys`, which an operator runs once no pod of a release
// before the sealed keys is left: with the pod's own configuration it
// seals every store's key still unsealed, deletes every unsealed record
// whose key is sealed and every sentinel, logs what it did, and exits.
// It exits non-zero when it left a store's records alone.
func sealSigningKeys(boot *slog.Logger, level *slog.LevelVar) error {
	cfg, log, err := configure(boot, level)
	if err != nil {
		return err
	}
	rdb, err := newRedis(cfg.RedisURL, log)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()
	keys, err := ucp.NewKeys(rdb, cfg.SigningKeyKEK)
	if err != nil {
		return err
	}

	ctx := context.Background()
	report, err := keys.SealUnsealed(ctx, true)
	log.InfoContext(ctx, "signing keys sealed",
		slog.String("sealed", strings.Join(report.Sealed, ",")),
		slog.String("already_sealed",
			strings.Join(report.AlreadySealed, ",")),
		slog.String("unsealed_deleted", strings.Join(report.Deleted, ",")))
	for _, schema := range slices.Sorted(maps.Keys(report.Conflicts)) {
		log.ErrorContext(ctx, "signing key left unsealed",
			slog.String(obs.KeyTenant, schema),
			slog.String("reason", report.Conflicts[schema]))
	}
	if err != nil {
		return err
	}
	if len(report.Conflicts) > 0 {
		return errSealConflicts
	}
	return nil
}
