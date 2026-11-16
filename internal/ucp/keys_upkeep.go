package ucp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// sealedKeyPattern matches every store's sealed signing key.
const sealedKeyPattern = "ag:*" + sealedKeySuffix

// ErrKEKOpensNoKey refuses a boot whose key-encryption key opens none of
// the sealed signing keys Redis holds: it is another environment's, or a
// changed one, and every store's profile would answer 503 and every
// webhook would stay pending.
var ErrKEKOpensNoKey = errors.New("ucp: SIGNING_KEY_ENCRYPTION_KEY opens " +
	"none of the stored signing keys; it is not the key that sealed them")

// CheckKEK opens every sealed signing key Redis holds, within ctx. It
// fails with ErrKEKOpensNoKey when at least one is stored and none opens,
// and logs each one that does not open by its store, never its contents.
// An environment that holds none passes: nothing to check against. It
// writes nothing, so a check that cannot run leaves no trace (no canary
// record, which would be as evictable as the keys).
func (k *Keys) CheckKEK(ctx context.Context, log *slog.Logger) error {
	opened, failed := 0, 0
	iter := k.rdb.Scan(ctx, 0, sealedKeyPattern, 1000).Iterator()
	for iter.Next(ctx) {
		name := iter.Val()
		schema := strings.TrimSuffix(strings.TrimPrefix(name, "ag:"),
			sealedKeySuffix)
		record, err := k.rdb.Get(ctx, name).Result()
		if errors.Is(err, redis.Nil) {
			continue // evicted since the scan
		}
		if err != nil {
			return fmt.Errorf("ucp: read the signing key of %s: %w",
				schema, err)
		}
		if _, err := k.open(schema, record); err != nil {
			failed++
			log.ErrorContext(ctx, "signing key does not open",
				slog.String(obs.KeyTenant, schema))
			continue
		}
		opened++
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("ucp: scan the signing keys: %w", err)
	}
	if opened == 0 && failed > 0 {
		return ErrKEKOpensNoKey
	}
	return nil
}

// kekCheckAttempt bounds one attempt of AwaitKEK; kekRetryFirst and
// kekRetryMax are its backoff between attempts that could not read Redis.
const (
	kekCheckAttempt = 30 * time.Second
	kekRetryFirst   = time.Second
	kekRetryMax     = 30 * time.Second
)

// errKEKUnchecked is what Checked reports until AwaitKEK has passed.
var errKEKUnchecked = errors.New("the signing keys are not checked yet")

// AwaitKEK runs CheckKEK until it reaches an outcome, as readiness waits
// for Redis: an attempt that cannot read Redis is logged at warn and
// retried, each bounded by kekCheckAttempt, with a backoff up to
// kekRetryMax. It returns nil once the check passed, and from then on
// Checked reports nil; ErrKEKOpensNoKey, the one definite failure, on
// which the process must exit; or ctx's error once ctx ends.
func (k *Keys) AwaitKEK(ctx context.Context, log *slog.Logger) error {
	wait := kekRetryFirst
	for {
		attempt, cancel := context.WithTimeout(ctx, kekCheckAttempt)
		err := k.CheckKEK(attempt, log)
		cancel()
		switch {
		case err == nil:
			k.checked.Store(true)
			return nil
		case errors.Is(err, ErrKEKOpensNoKey):
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		}
		log.WarnContext(ctx, "signing keys not checked yet; retrying",
			slog.String("error", err.Error()),
			slog.Duration("retry_in", wait))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, kekRetryMax)
	}
}

// Checked reports whether AwaitKEK has passed: nil once it has. The pod
// is not ready until then (obs.Readyz).
func (k *Keys) Checked() error {
	if k.checked.Load() {
		return nil
	}
	return errKEKUnchecked
}

// keyRefreshInterval paces KeepFresh.
const keyRefreshInterval = 5 * time.Minute

// refreshBatch is how many records one MGET of KeepFresh reads.
const refreshBatch = 100

// KeepFresh runs Refresh every keyRefreshInterval until ctx ends.
func (k *Keys) KeepFresh(ctx context.Context, log *slog.Logger) {
	tick := time.NewTicker(keyRefreshInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			k.Refresh(ctx, log)
		}
	}
}

// Refresh reads the record of every key this pod holds. The Redis is
// shared and evicts the least recently used keys; a pod reads a store's
// record once and then signs from memory, so without this the record
// looks idle and goes first. The read keeps it recent. A record gone
// anyway is written back from the key the pod holds, so an eviction never
// becomes a new key; a record holding another key (minted by a pod that
// found none) is adopted, logged at error, so every pod converges on the
// stored one. A record that does not open is logged and left alone. The
// work is one MGET per refreshBatch keys held, at most maxKeyEntries.
func (k *Keys) Refresh(ctx context.Context, log *slog.Logger) {
	k.mu.RLock()
	schemas := make([]string, 0, len(k.mem))
	for schema := range k.mem {
		schemas = append(schemas, schema)
	}
	k.mu.RUnlock()
	slices.Sort(schemas)

	for batch := range slices.Chunk(schemas, refreshBatch) {
		names := make([]string, len(batch))
		for i, schema := range batch {
			names[i] = signingKeyRedisKey(schema)
		}
		records, err := k.rdb.MGet(ctx, names...).Result()
		if err != nil {
			if ctx.Err() == nil {
				log.WarnContext(ctx, "signing keys not refreshed",
					slog.String("error", err.Error()))
			}
			return
		}
		for i, schema := range batch {
			record, _ := records[i].(string)
			k.refreshOne(ctx, log, schema, record)
		}
	}
}

// refreshOne settles one store's record against the key the pod holds.
func (k *Keys) refreshOne(
	ctx context.Context, log *slog.Logger, schema, record string,
) {
	k.mu.RLock()
	held, ok := k.mem[schema]
	k.mu.RUnlock()
	if !ok {
		return
	}
	scalar, err := held.Private.Bytes()
	if err != nil {
		return
	}
	if record == "" {
		written, err := k.rdb.SetNX(ctx, signingKeyRedisKey(schema),
			k.seal(schema, scalar), 0).Result()
		switch {
		case err != nil:
			log.WarnContext(ctx, "signing key not written back",
				slog.String(obs.KeyTenant, schema),
				slog.String("error", err.Error()))
		case written:
			log.WarnContext(ctx, "signing key record was gone; written "+
				"back from the key this pod holds",
				slog.String(obs.KeyTenant, schema))
		}
		// Not written: another pod wrote one meanwhile; the next refresh
		// compares it.
		return
	}
	stored, err := k.open(schema, record)
	if err != nil {
		log.ErrorContext(ctx, "signing key does not open",
			slog.String(obs.KeyTenant, schema))
		return
	}
	if bytes.Equal(stored, scalar) {
		return
	}
	key, err := keyFromScalar(stored)
	if err != nil {
		log.ErrorContext(ctx, "signing key does not open",
			slog.String(obs.KeyTenant, schema))
		return
	}
	log.ErrorContext(ctx, "signing key differs from the stored one; "+
		"adopting the stored key",
		slog.String(obs.KeyTenant, schema),
		slog.String("held_kid", held.KID), slog.String("stored_kid", key.KID))
	k.mu.Lock()
	k.mem[schema] = key
	k.mu.Unlock()
}
