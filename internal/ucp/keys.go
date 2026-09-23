// Package ucp implements the Universal Commerce Protocol surface: the
// /.well-known/ucp business profile, checkout payload rendering for the
// MCP tool binding, and signed order webhooks to platforms.
package ucp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
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
const Version = "2026-08-25"

// signingKeyRedisKey names the stored ES256 private scalar. The algorithm
// is part of the name because the value is a bare 32-byte scalar: a key
// of another algorithm under the same name would parse just as silently.
func signingKeyRedisKey(schema string) string {
	return "ag:" + schema + ":ucp:signing_key:es256"
}

// maxKeyEntries bounds the in-process key cache the same way the tenant
// resolver bounds its map: real schema counts are tiny, the bound only
// guards against cardinality abuse.
const maxKeyEntries = 10_000

// p256FieldBytes is the byte length of a P-256 coordinate and scalar.
const p256FieldBytes = 32

// SigningKey is one tenant's ECDSA P-256 key pair. It signs that tenant's
// order webhooks (ES256 under RFC 9421) and its public half is published
// only in that tenant's /.well-known/ucp profile.
//
// ES256 because UCP makes it the one algorithm every verifier MUST
// support; Ed25519 support is optional, and a platform on the baseline
// could not verify an EdDSA webhook at all.
type SigningKey struct {
	Private *ecdsa.PrivateKey
	KID     string
	x, y    []byte
}

// JWK renders the public key per RFC 7517/7518 (EC, P-256).
func (k *SigningKey) JWK() map[string]string {
	return map[string]string{
		"kid": k.KID,
		"kty": "EC",
		"crv": "P-256",
		"alg": "ES256",
		"use": "sig",
		"x":   base64.RawURLEncoding.EncodeToString(k.x),
		"y":   base64.RawURLEncoding.EncodeToString(k.y),
	}
}

// KeyID implements httpsig.Signer.
func (k *SigningKey) KeyID() string { return k.KID }

// Sign implements httpsig.Signer: ES256 over the signature base, encoded
// as the fixed-width r||s RFC 9421 section 3.3.4 requires — not ASN.1.
func (k *SigningKey) Sign(base []byte) ([]byte, error) {
	digest := sha256.Sum256(base)
	r, s, err := ecdsa.Sign(rand.Reader, k.Private, digest[:])
	if err != nil {
		return nil, err
	}
	sig := make([]byte, 2*p256FieldBytes)
	r.FillBytes(sig[:p256FieldBytes])
	s.FillBytes(sig[p256FieldBytes:])
	return sig, nil
}

// Keys loads per-schema signing keys lazily, caching them in process —
// keys are immutable once minted, so cached entries never go stale.
type Keys struct {
	rdb *redis.Client

	mu  sync.RWMutex
	mem map[string]*SigningKey
}

func NewKeys(rdb *redis.Client) *Keys {
	return &Keys{rdb: rdb, mem: make(map[string]*SigningKey)}
}

// ForSchema returns the tenant's persistent signing key, minting and
// storing one on first use.
//
// Losing the Redis value is an UNGRACEFUL rotation. UCP verifiers pin
// nothing — they re-fetch keys[] from the profile — but the spec's
// rotation procedure publishes the new key ALONGSIDE the old for a grace
// period (>=7 days) so in-flight signatures still verify, and the profile
// publishes exactly one key per tenant. The key therefore carries no
// TTL, and a graceful rotation would need keys[] to hold two.
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
	encoded, err := k.rdb.Get(ctx, redisKey).Result()
	switch {
	case errors.Is(err, redis.Nil):
		minted, err := newEncodedKey()
		if err != nil {
			return nil, err
		}
		// SETNX so concurrent pods agree on one key; a lost race
		// re-reads the winner.
		ok, err := k.rdb.SetNX(ctx, redisKey, minted, 0).Result()
		if err != nil {
			return nil, fmt.Errorf("ucp: persist key: %w", err)
		}
		if !ok {
			return k.loadOrCreate(ctx, schema)
		}
		encoded = minted
	case err != nil:
		return nil, fmt.Errorf("ucp: load key: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("ucp: corrupt key: %w", err)
	}
	return keyFromScalar(raw)
}

// newEncodedKey mints the base64 private scalar to persist for a schema
// with no key yet. Every schema mints the same way: no tenant carries a
// pre-existing identity, so onboarding the thousandth store runs the code
// path the first one did.
func newEncodedKey() (string, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("ucp: generate key: %w", err)
	}
	raw, err := priv.Bytes()
	if err != nil {
		return "", fmt.Errorf("ucp: encode key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func keyFromScalar(raw []byte) (*SigningKey, error) {
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, fmt.Errorf("ucp: corrupt key: %w", err)
	}
	point, err := priv.PublicKey.Bytes() // 0x04 || X || Y
	if err != nil {
		return nil, fmt.Errorf("ucp: public key: %w", err)
	}
	x := point[1 : 1+p256FieldBytes]
	y := point[1+p256FieldBytes:]
	kid, err := jwkThumbprint(x, y)
	if err != nil {
		return nil, err
	}
	return &SigningKey{Private: priv, KID: kid, x: x, y: y}, nil
}

// jwkThumbprint computes the RFC 7638 SHA-256 thumbprint of the EC JWK:
// the required members crv, kty, x, y in lexicographic order, which
// encoding/json's sorted map keys produce.
func jwkThumbprint(x, y []byte) (string, error) {
	canonical, err := json.Marshal(map[string]string{
		"crv": "P-256",
		"kty": "EC",
		"x":   base64.RawURLEncoding.EncodeToString(x),
		"y":   base64.RawURLEncoding.EncodeToString(y),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}
