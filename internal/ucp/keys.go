// Package ucp implements the Universal Commerce Protocol surface: the
// /.well-known/ucp business profile, checkout payload rendering for the
// MCP tool binding, and signed order webhooks to platforms.
package ucp

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/redis/go-redis/v9"
)

// Version is the implemented UCP specification version.
const Version = "2026-08-25"

// signingKeyRedisKey names a store's sealed ES256 private scalar. The
// algorithm is part of the name because the value is a bare 32-byte
// scalar: a key of another algorithm under the same name would parse
// just as silently. So is the sealing (AES-256-GCM), so a pod that reads
// another encoding never shares the record.
func signingKeyRedisKey(schema string) string {
	return "ag:" + schema + sealedKeySuffix
}

// sealedKeySuffix ends the name of every store's sealed signing key.
const sealedKeySuffix = ":ucp:signing_key:es256:aes256gcm"

// sealPurpose opens the associated data of every sealed signing key.
const sealPurpose = "grooveshop-agent-gateway/ucp-signing-key/es256"

// sealContext is the associated data a store's sealed key is bound to:
// the purpose and the store. A record copied to another store's key, or
// read for another purpose, does not open. The purpose is fixed and the
// NUL never occurs in a schema name, so the store is read unambiguously.
func sealContext(schema string) []byte {
	return []byte(sealPurpose + "\x00" + schema)
}

// kekBytes is the length of the key-encryption key: AES-256.
const kekBytes = 32

// mintScript stores a newly minted sealed key ARGV[1] at KEYS[1] and
// returns 1, unless a key is there (0) or the store's unsealed record
// KEYS[2] holds a key an earlier release minted (-1): that key is the
// store's, and is sealed instead (sealUnsealedOf). With the sealed key it
// writes the sentinel ARGV[2] to KEYS[2] where nothing is, so a pod of an
// earlier release finds a record it cannot decode, and answers 503,
// instead of minting a second key for the store. The sentinel expires
// after ARGV[3] ms (unsealedSentinelTTL). A run repeated because
// its reply was lost finds its own record and returns 0, and the caller
// reads that record.
//
// KEYS[2] and the sentinel go with the unsealed record (keys_unsealed.go).
var mintScript = redis.NewScript(`
local unsealed = redis.call('GET', KEYS[2])
if unsealed and unsealed ~= ARGV[2] then
  return -1
end
if redis.call('SET', KEYS[1], ARGV[1], 'NX') then
  redis.call('SET', KEYS[2], ARGV[2], 'NX', 'PX', ARGV[3])
  return 1
end
return 0
`)

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
//
// Redis holds each key sealed with AES-256-GCM under the key-encryption
// key (SIGNING_KEY_ENCRYPTION_KEY), bound to its store (sealContext):
// Redis, its AOF files and their backups sit outside the process, and
// the scalar signs a store's order webhooks. Only the process memory
// holds it in clear. The records carry no TTL; the gateway's Redis does
// not evict (noeviction).
type Keys struct {
	rdb  *redis.Client
	aead cipher.AEAD
	// checked is set once AwaitKEK has passed: readiness waits for it.
	checked atomic.Bool

	mu  sync.RWMutex
	mem map[string]*SigningKey
}

// NewKeys builds the store of signing keys sealed under kek, an AES-256
// key (config.Config.SigningKeyKEK).
func NewKeys(rdb *redis.Client, kek []byte) (*Keys, error) {
	if len(kek) != kekBytes {
		return nil, fmt.Errorf(
			"ucp: the key-encryption key must be %d bytes", kekBytes)
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("ucp: key-encryption key: %w", err)
	}
	// A random 96-bit nonce per seal, prepended to the record: one seal
	// per store's key is far below the 2^32 seals a key allows.
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("ucp: key-encryption key: %w", err)
	}
	return &Keys{rdb: rdb, aead: aead, mem: make(map[string]*SigningKey)}, nil
}

// seal returns the record Redis holds for schema's scalar.
func (k *Keys) seal(schema string, scalar []byte) string {
	return base64.StdEncoding.EncodeToString(
		k.aead.Seal(nil, nil, scalar, sealContext(schema)))
}

// open returns the scalar a record sealed for schema holds. It fails for
// a record sealed for another store, under another key-encryption key,
// or changed since, and names no key material.
func (k *Keys) open(schema, record string) ([]byte, error) {
	sealed, err := base64.StdEncoding.DecodeString(record)
	if err != nil {
		return nil, errors.New("ucp: sealed signing key is not base64")
	}
	scalar, err := k.aead.Open(nil, nil, sealed, sealContext(schema))
	if err != nil {
		return nil, errors.New("ucp: sealed signing key does not open: " +
			"another SIGNING_KEY_ENCRYPTION_KEY sealed it, or it is " +
			"another store's or changed")
	}
	return scalar, nil
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
	record, err := k.rdb.Get(ctx, redisKey).Result()
	switch {
	case errors.Is(err, redis.Nil):
		scalar, err := newScalar()
		if err != nil {
			return nil, err
		}
		// Set only where no key is, so concurrent pods agree on one key;
		// a lost race re-reads the winner.
		stored, err := mintScript.Run(ctx, k.rdb,
			[]string{redisKey, unsealedKeyRedisKey(schema)},
			k.seal(schema, scalar), unsealedSentinel,
			unsealedSentinelTTL.Milliseconds()).Int()
		switch {
		case err != nil:
			return nil, fmt.Errorf("ucp: persist key: %w", err)
		case stored < 0:
			// A pod of an earlier release minted the store's key: seal
			// that key, never a new one, then read it.
			if err := k.sealUnsealedOf(ctx, schema); err != nil {
				return nil, err
			}
			return k.loadOrCreate(ctx, schema)
		case stored == 0:
			return k.loadOrCreate(ctx, schema)
		}
		return SigningKeyFromScalar(scalar)
	case err != nil:
		return nil, fmt.Errorf("ucp: load key: %w", err)
	}
	scalar, err := k.open(schema, record)
	if err != nil {
		return nil, err
	}
	return SigningKeyFromScalar(scalar)
}

// newScalar mints the private scalar of a schema with no key yet. Every
// schema mints the same way: no tenant carries a pre-existing identity,
// so onboarding the thousandth store runs the code path the first one
// did.
func newScalar() ([]byte, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ucp: generate key: %w", err)
	}
	raw, err := priv.Bytes()
	if err != nil {
		return nil, fmt.Errorf("ucp: encode key: %w", err)
	}
	return raw, nil
}

// SigningKeyFromScalar is the ES256 key whose private scalar is raw (the
// fixed-width big-endian encoding ecdsa.PrivateKey.Bytes gives), with
// its kid the public key's RFC 7638 thumbprint.
func SigningKeyFromScalar(raw []byte) (*SigningKey, error) {
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
	kid := thumbprint(map[string]string{
		"crv": "P-256",
		"kty": "EC",
		"x":   base64.RawURLEncoding.EncodeToString(x),
		"y":   base64.RawURLEncoding.EncodeToString(y),
	})
	return &SigningKey{Private: priv, KID: kid, x: x, y: y}, nil
}

// thumbprint is the RFC 7638 SHA-256 thumbprint of a JWK, given its
// required members: encoding/json writes a map's keys in lexicographic
// order and without whitespace, as the thumbprint's input is written.
func thumbprint(required map[string]string) string {
	canonical, _ := json.Marshal(required) // a map of strings always encodes
	sum := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
