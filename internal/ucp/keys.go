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

	"github.com/redis/go-redis/v9"
)

// Version is the implemented UCP specification version.
const Version = "2026-04-08"

const signingKeyRedisKey = "ag:ucp:signing_key"

// SigningKey is the platform-wide Ed25519 key pair used to sign order
// webhooks; its public half is published in every tenant profile.
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

// LoadOrCreateSigningKey returns the persistent signing key, minting and
// storing one on first use. Losing the Redis value is a key rotation:
// platforms re-read the profile's JWK set, so rotation is safe, just not
// free — the key has no TTL for that reason.
func LoadOrCreateSigningKey(
	ctx context.Context, rdb *redis.Client,
) (*SigningKey, error) {
	seedB64, err := rdb.Get(ctx, signingKeyRedisKey).Result()
	switch {
	case errors.Is(err, redis.Nil):
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("ucp: generate key: %w", err)
		}
		seed := priv.Seed()
		encoded := base64.StdEncoding.EncodeToString(seed)
		// SETNX so two booting pods agree on one key.
		ok, err := rdb.SetNX(ctx, signingKeyRedisKey, encoded, 0).Result()
		if err != nil {
			return nil, fmt.Errorf("ucp: persist key: %w", err)
		}
		if !ok {
			return LoadOrCreateSigningKey(ctx, rdb)
		}
		return fromSeed(seed)
	case err != nil:
		return nil, fmt.Errorf("ucp: load key: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		return nil, fmt.Errorf("ucp: corrupt key: %w", err)
	}
	return fromSeed(seed)
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
