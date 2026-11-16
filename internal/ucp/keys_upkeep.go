package ucp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// sealedKeyPattern matches every store's sealed signing key.
const sealedKeyPattern = "ag:*" + sealedKeySuffix

// kekScanCount is the COUNT hint of each SCAN of the check.
const kekScanCount = 1000

// ErrKEKOpensNoKey refuses a boot whose key-encryption key opens none of
// the sealed signing keys Redis holds: it is another environment's, or a
// changed one, and every store's profile would answer 503 and every
// webhook would stay pending.
var ErrKEKOpensNoKey = errors.New("ucp: SIGNING_KEY_ENCRYPTION_KEY opens " +
	"none of the stored signing keys; it is not the key that sealed them")

// kekCheckAttempt bounds one attempt of AwaitKEK; kekRetryFirst and
// kekRetryMax are its backoff between attempts that could not read Redis.
const (
	kekCheckAttempt = 30 * time.Second
	kekRetryFirst   = time.Second
	kekRetryMax     = 30 * time.Second
)

// errKEKUnchecked is what Checked reports until AwaitKEK has passed.
var errKEKUnchecked = errors.New("the signing keys are not checked yet")

// kekScan is how far the check has scanned the sealed records, and what
// it found. The cursor is kept across an attempt that ran out of time,
// so a scan longer than one attempt still completes; after any other
// failure it starts again from 0, because a cursor taken before Redis
// restarted can skip keys (Redis seeds its hash afresh at each start).
// What was found stays: a key that opened did open.
type kekScan struct {
	cursor         uint64
	opened, failed bool
}

// AwaitKEK opens every sealed signing key Redis holds, and waits for
// Redis as readiness does. It returns nil once the scan completed with
// a key that opens, or with none stored (a fresh environment, nothing to
// check against), and from then on Checked reports nil. It returns
// ErrKEKOpensNoKey, the one definite failure, on which the process must
// exit, when keys are stored and none opens; or ctx's error once ctx
// ends. Each record that does not open is logged by its store, never its
// contents.
//
// An attempt that cannot read Redis is logged at warn and retried with a
// backoff up to kekRetryMax. Each attempt is bounded by kekCheckAttempt;
// one that ran out of time goes on from its cursor in the next, so the
// scan completes however many records there are, and one that failed
// otherwise scans again from the start (kekScan). It writes nothing (no
// canary record).
func (k *Keys) AwaitKEK(ctx context.Context, log *slog.Logger) error {
	var scan kekScan
	wait := kekRetryFirst
	for {
		attempt, cancel := context.WithTimeout(ctx, kekCheckAttempt)
		err := k.scanKEK(attempt, log, &scan)
		// Only the attempt's own deadline keeps the cursor: a Redis
		// failure that came back as the deadline passed may still have
		// been a restart.
		outOfTime := errors.Is(err, context.DeadlineExceeded)
		cancel()
		if err != nil && (!outOfTime || ctx.Err() != nil) {
			scan.cursor = 0
		}
		switch {
		case err == nil && scan.failed && !scan.opened:
			return ErrKEKOpensNoKey
		case err == nil:
			k.checked.Store(true)
			return nil
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

// scanKEK goes on scanning from scan's cursor until the scan is complete
// (nil) or a Redis call fails. The cursor moves past a batch only once
// every record of it was read, so a batch cut short is read again.
func (k *Keys) scanKEK(
	ctx context.Context, log *slog.Logger, scan *kekScan,
) error {
	for {
		names, next, err := k.rdb.Scan(ctx, scan.cursor, sealedKeyPattern,
			kekScanCount).Result()
		if err != nil {
			return fmt.Errorf("ucp: scan the signing keys: %w", err)
		}
		for _, name := range names {
			schema := strings.TrimSuffix(strings.TrimPrefix(name, "ag:"),
				sealedKeySuffix)
			record, err := k.rdb.Get(ctx, name).Result()
			if errors.Is(err, redis.Nil) {
				continue // deleted since the scan
			}
			if err != nil {
				return fmt.Errorf("ucp: read the signing key of %s: %w",
					schema, err)
			}
			if _, err := k.open(schema, record); err != nil {
				scan.failed = true
				log.ErrorContext(ctx, "signing key does not open",
					slog.String(obs.KeyTenant, schema))
				continue
			}
			scan.opened = true
		}
		scan.cursor = next
		if next == 0 {
			return nil
		}
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
