package ucp

// The move of the stores' signing keys from the unsealed record to the
// sealed one. Releases before this one stored each key unsealed, under
// unsealedKeyRedisKey, and its public half is published in the store's
// profile, where platforms verify its order webhooks against it. The
// move keeps every key: the scalar is sealed as it is, so the key id and
// the published key do not change.
//
// Delete this file, mintScript's check of the unsealed record and the
// gateway's seal command in the release after every environment has run
// the command (docs/agent-protocols-runbook.md, section 12).

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

// deleteIfScript deletes KEYS[1] while it holds ARGV[1].
var deleteIfScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
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
	// Deleted are the stores whose unsealed record it deleted.
	Deleted []string
	// Conflicts are the stores it left as they are, each with why: an
	// unsealed record that is not a key, or a sealed record that holds
	// another key or does not open.
	Conflicts map[string]string
}

// SealUnsealed seals every store's key still held in an unsealed record,
// writing the sealed record where none is. With deleteUnsealed it then
// deletes each unsealed record whose key the sealed record holds; run
// that only once no pod of an earlier release is left, as those read
// and mint the unsealed record only.
//
// A store whose sealed record holds another key keeps both records and
// is reported: the pods of this release publish the sealed one. That
// happens only for a store whose first key was minted by an earlier pod
// after this release's pods had sealed the others; mintScript refuses
// that case, so it is the operator's to settle.
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
		scalar, err := base64.StdEncoding.DecodeString(plain)
		if err == nil {
			_, err = keyFromScalar(scalar)
		}
		if err != nil {
			report.Conflicts[schema] = "the unsealed record is not a key"
			continue
		}

		stored, err := k.rdb.SetNX(ctx, signingKeyRedisKey(schema),
			k.seal(schema, scalar), 0).Result()
		if err != nil {
			return report, fmt.Errorf("ucp: seal %s: %w", schema, err)
		}
		if stored {
			report.Sealed = append(report.Sealed, schema)
		} else {
			record, err := k.rdb.Get(ctx, signingKeyRedisKey(schema)).Result()
			if err != nil {
				return report, fmt.Errorf("ucp: read the sealed key of %s: %w",
					schema, err)
			}
			if why := k.notHolding(schema, record, scalar); why != "" {
				report.Conflicts[schema] = why
				continue
			}
			report.AlreadySealed = append(report.AlreadySealed, schema)
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
