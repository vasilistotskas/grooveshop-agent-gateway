package ucp

// The move of the stores' signing keys from the unsealed record to the
// sealed one. Releases before this one stored each key unsealed, under
// unsealedKeyRedisKey, and its public half is published in the store's
// profile, where platforms verify its order webhooks against it. The
// move keeps every key: the scalar is sealed as it is, so the key id and
// the published key do not change.
//
// - A pod of this release seals a store's unsealed key the first time it
//   needs the key (loadOrCreate, sealUnsealedOf), never minting another.
// - A key it mints for a store with none leaves unsealedSentinel in the
//   unsealed record, so a pod of an earlier release cannot mint a second
//   key for that store during the rollout (mintScript).
// - The gateway's seal command seals every key still unsealed and
//   deletes the unsealed records, sentinels included, once no pod of an
//   earlier release is left (SealUnsealed).
//
// Delete this file, mintScript's unsealed record and the gateway's seal
// command in the release after every environment has run the command
// (docs/agent-protocols-runbook.md, section 12).

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// SealCommand is the gateway's subcommand that seals every unsealed key
// and deletes the unsealed records (cmd/gateway).
const SealCommand = "seal-signing-keys"

// unsealedKeySuffix ends the name of an unsealed record.
const unsealedKeySuffix = ":ucp:signing_key:es256"

// unsealedKeyRedisKey names the record releases before the sealed one
// stored a store's key in: the base64 scalar, unsealed.
func unsealedKeyRedisKey(schema string) string {
	return "ag:" + schema + unsealedKeySuffix
}

// unsealedSentinel is what the unsealed record of a store whose key this
// release minted holds. It is not base64 (spaces), so a pod of an earlier
// release fails to decode it and answers 503, and its SETNX finds the
// record taken: it never mints a second key for the store.
const unsealedSentinel = "sealed by a later release: not a key"

// errUnsealedNotAKey reports an unsealed record that holds neither a key
// nor the sentinel. The store is refused, never given a new key.
var errUnsealedNotAKey = errors.New(
	"ucp: the store's unsealed signing key record is not a key")

// deleteIfScript deletes KEYS[1] while it holds ARGV[1], and reports 1
// when the key is gone: deleted by this run, or by an earlier run of it
// whose reply was lost and which go-redis sent again.
var deleteIfScript = redis.NewScript(`
local held = redis.call('GET', KEYS[1])
if not held then
  return 1
end
if held == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// SealReport is what SealUnsealed did, store by store.
type SealReport struct {
	// Sealed are the stores whose key it sealed.
	Sealed []string
	// AlreadySealed are the stores whose sealed record already held the
	// same key.
	AlreadySealed []string
	// Deleted are the stores whose unsealed record (a key or the
	// sentinel) it deleted.
	Deleted []string
	// Conflicts are the stores it left as they are, each with why: an
	// unsealed record that is not a key, or a sealed record that holds
	// another key or does not open.
	Conflicts map[string]string
}

// sealUnsealedOf seals the key schema's unsealed record holds where no
// sealed record is. A record gone since, or holding the sentinel, leaves
// nothing to seal.
func (k *Keys) sealUnsealedOf(ctx context.Context, schema string) error {
	plain, err := k.rdb.Get(ctx, unsealedKeyRedisKey(schema)).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return nil
	case err != nil:
		return fmt.Errorf("ucp: read the unsealed key: %w", err)
	case plain == unsealedSentinel:
		return nil
	}
	_, _, err = k.sealPlain(ctx, schema, plain)
	return err
}

// sealPlain writes the sealed record of the key plain, an unsealed
// record's value, unless a sealed record exists. It returns the key's
// scalar and whether it wrote the record.
func (k *Keys) sealPlain(
	ctx context.Context, schema, plain string,
) ([]byte, bool, error) {
	scalar, err := base64.StdEncoding.DecodeString(plain)
	if err == nil {
		_, err = keyFromScalar(scalar)
	}
	if err != nil {
		return nil, false, errUnsealedNotAKey
	}
	stored, err := k.rdb.SetNX(ctx, signingKeyRedisKey(schema),
		k.seal(schema, scalar), 0).Result()
	if err != nil {
		return nil, false, fmt.Errorf("ucp: seal the key of %s: %w",
			schema, err)
	}
	return scalar, stored, nil
}

// SealUnsealed seals every store's key still held in an unsealed record,
// writing the sealed record where none is. With deleteUnsealed it then
// deletes each unsealed record whose key the sealed record holds, and
// every sentinel; run that only once no pod of an earlier release is
// left, as those read and mint the unsealed record only.
//
// A store whose sealed record holds another key keeps both records and
// is reported. mintScript leaves no way to reach that state while both
// records live; it takes an unsealed record deleted, or a sealed one
// evicted, while a pod of an earlier release still runs.
func (k *Keys) SealUnsealed(
	ctx context.Context, deleteUnsealed bool,
) (SealReport, error) {
	report := SealReport{Conflicts: map[string]string{}}
	iter := k.rdb.Scan(ctx, 0, "ag:*"+unsealedKeySuffix, 1000).Iterator()
	for iter.Next(ctx) {
		name := iter.Val()
		schema := strings.TrimSuffix(strings.TrimPrefix(name, "ag:"),
			unsealedKeySuffix)
		if schema == "" {
			continue
		}
		plain, err := k.rdb.Get(ctx, name).Result()
		if errors.Is(err, redis.Nil) {
			continue // deleted since the scan
		}
		if err != nil {
			return report, fmt.Errorf("ucp: read %s: %w", schema, err)
		}

		if plain != unsealedSentinel {
			scalar, stored, err := k.sealPlain(ctx, schema, plain)
			switch {
			case errors.Is(err, errUnsealedNotAKey):
				report.Conflicts[schema] = "the unsealed record is not a key"
				continue
			case err != nil:
				return report, err
			case stored:
				report.Sealed = append(report.Sealed, schema)
			default:
				record, err := k.rdb.Get(ctx,
					signingKeyRedisKey(schema)).Result()
				if err != nil {
					return report, fmt.Errorf(
						"ucp: read the sealed key of %s: %w", schema, err)
				}
				if why := k.notHolding(schema, record, scalar); why != "" {
					report.Conflicts[schema] = why
					continue
				}
				report.AlreadySealed = append(report.AlreadySealed, schema)
			}
		}

		if !deleteUnsealed {
			continue
		}
		deleted, err := deleteIfScript.Run(ctx, k.rdb, []string{name},
			plain).Int()
		if err != nil {
			return report, fmt.Errorf("ucp: delete %s: %w", schema, err)
		}
		if deleted == 1 {
			report.Deleted = append(report.Deleted, schema)
		}
	}
	if err := iter.Err(); err != nil {
		return report, fmt.Errorf("ucp: scan unsealed keys: %w", err)
	}
	return report, nil
}

// notHolding reports why record, schema's sealed record, does not hold
// scalar, or "" when it does.
func (k *Keys) notHolding(schema, record string, scalar []byte) string {
	sealed, err := k.open(schema, record)
	if err != nil {
		return err.Error()
	}
	if subtle.ConstantTimeCompare(sealed, scalar) != 1 {
		return "the sealed record holds another key"
	}
	return ""
}
