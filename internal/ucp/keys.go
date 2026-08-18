// Package ucp implements the Universal Commerce Protocol surface: the
// /.well-known/ucp business profile, checkout payload rendering for the
// MCP tool binding, and signed order webhooks to platforms.
package ucp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"
)

// Version is the implemented UCP specification version.
const Version = "2026-04-08"

// legacySigningKeyRedisKey held the single platform-wide key before keys
// went per-schema. It historically signed for webside (tenant #1), so its
// seed is adopted (copied) into webside's per-schema key on first load to
// keep the platform-registered public key verifying. The legacy key is
// never written or deleted here — remove it manually after cutover.
const legacySigningKeyRedisKey = "ag:ucp:signing_key"

const legacyKeySchema = "webside"

func signingKeyRedisKey(schema string) string {
	return "ag:" + schema + ":ucp:signing_key"
}

// maxKeyEntries bounds the in-process key cache the same way the tenant
// resolver bounds its map: real schema counts are tiny, the bound only
// guards against cardinality abuse.
const maxKeyEntries = 10_000

// SigningKey is one tenant's Ed25519 key pair. It signs that tenant's
// order webhooks and its public half is published only in that tenant's
// /.well-known/ucp profile.
type SigningKey struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
	KID     string
}

// JWK renders the public key per RFC 7517 (OKP/Ed25519).
func (k *SigningKey) JWK() map[string]string {
	return map[string]string{
		"kid": k.KID,
		"kty": "OKP",
		"crv": "Ed25519",
		"alg": "EdDSA",
		"use": "sig",
		"x":   base64.RawURLEncoding.EncodeToString(k.Public),
	}
}

// Keys loads per-schema signing keys lazily, caching them in process —
// seeds are immutable once minted, so cached entries never go stale.
type Keys struct {
	rdb *redis.Client

	mu  sync.RWMutex
	mem map[string]*SigningKey
}

func NewKeys(rdb *redis.Client) *Keys {
	return &Keys{rdb: rdb, mem: make(map[string]*SigningKey)}
}

// ForSchema returns the tenant's persistent signing key, minting and
// storing one at ag:{schema}:ucp:signing_key on first use. Losing the
// Redis value is a key rotation: platforms re-read the profile's JWK set,
// so rotation is safe, just not free — the key has no TTL for that reason.
func (k *Keys) ForSchema(
	ctx context.Context, schema string,
) (*SigningKey, error) {
	if schema == "" {
		return nil, errors.New("ucp: empty schema")
	}
	k.mu.RLock()
	key, ok := k.mem[schema]
	k.mu.RUnlock()
	if ok {
		return key, nil
	}
	key, err := k.loadOrCreate(ctx, schema)
	if err != nil {
		return nil, err
	}
	k.remember(schema, key)
	return key, nil
}

func (k *Keys) remember(schema string, key *SigningKey) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.mem) >= maxKeyEntries {
		// Map iteration order is random, so this evicts an arbitrary entry.
		for s := range k.mem {
			delete(k.mem, s)
			break
		}
	}
	k.mem[schema] = key
}

func (k *Keys) loadOrCreate(
	ctx context.Context, schema string,
) (*SigningKey, error) {
	redisKey := signingKeyRedisKey(schema)
	seedB64, err := k.rdb.Get(ctx, redisKey).Result()
	switch {
	case errors.Is(err, redis.Nil):
		encoded, err := k.newSeed(ctx, schema)
		if err != nil {
			return nil, err
		}
		// SETNX so concurrent pods (and the legacy adoption) agree on one
		// key; a lost race re-reads the winner.
		ok, err := k.rdb.SetNX(ctx, redisKey, encoded, 0).Result()
		if err != nil {
			return nil, fmt.Errorf("ucp: persist key: %w", err)
		}
		if !ok {
			return k.loadOrCreate(ctx, schema)
		}
		seedB64 = encoded
	case err != nil:
		return nil, fmt.Errorf("ucp: load key: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		return nil, fmt.Errorf("ucp: corrupt key: %w", err)
	}
	return fromSeed(seed)
}

// newSeed returns the base64 seed to persist for a schema with no key yet:
// the legacy global seed for the schema that historically used it, a
// freshly minted one otherwise.
func (k *Keys) newSeed(ctx context.Context, schema string) (string, error) {
	if schema == legacyKeySchema {
		encoded, err := k.rdb.Get(ctx, legacySigningKeyRedisKey).Result()
		switch {
		case err == nil:
			return encoded, nil
		case !errors.Is(err, redis.Nil):
			return "", fmt.Errorf("ucp: load legacy key: %w", err)
		}
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("ucp: generate key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(priv.Seed()), nil
}

func fromSeed(seed []byte) (*SigningKey, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("ucp: bad seed length")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	kid, err := jwkThumbprint(pub)
	if err != nil {
		return nil, err
	}
	return &SigningKey{Private: priv, Public: pub, KID: kid}, nil
}

// jwkThumbprint computes the RFC 7638 SHA-256 thumbprint of the OKP JWK
// (lexicographically ordered required members).
func jwkThumbprint(pub ed25519.PublicKey) (string, error) {
	canonical, err := json.Marshal(map[string]string{
		"crv": "Ed25519",
		"kty": "OKP",
		"x":   base64.RawURLEncoding.EncodeToString(pub),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}
