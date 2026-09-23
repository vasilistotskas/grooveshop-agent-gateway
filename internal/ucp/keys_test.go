package ucp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// publicKeyFromJWK rebuilds the verifier's view of a key from nothing but
// the published JWK, the way a platform does.
func publicKeyFromJWK(t *testing.T, jwk map[string]string) *ecdsa.PublicKey {
	t.Helper()
	require.Equal(t, "EC", jwk["kty"])
	require.Equal(t, "P-256", jwk["crv"])
	x, err := base64.RawURLEncoding.DecodeString(jwk["x"])
	require.NoError(t, err)
	y, err := base64.RawURLEncoding.DecodeString(jwk["y"])
	require.NoError(t, err)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(),
		append(append([]byte{0x04}, x...), y...))
	require.NoError(t, err)
	return pub
}

// ES256 signatures verify against the published JWK and use the
// fixed-width r||s encoding RFC 9421 requires, never ASN.1.
func TestSigningKeySignsES256VerifiableFromTheJWK(t *testing.T) {
	key := testKey(t)
	base := []byte(`"@method": POST`)
	sig, err := key.Sign(base)
	require.NoError(t, err)
	require.Len(t, sig, 64)

	digest := sha256.Sum256(base)
	pub := publicKeyFromJWK(t, key.JWK())
	assert.True(t, ecdsa.Verify(pub, digest[:],
		new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])))
}

// The key id is the RFC 7638 thumbprint, recomputable from the JWK.
func TestSigningKeyIDIsTheJWKThumbprint(t *testing.T) {
	key := testKey(t)
	x, err := base64.RawURLEncoding.DecodeString(key.JWK()["x"])
	require.NoError(t, err)
	y, err := base64.RawURLEncoding.DecodeString(key.JWK()["y"])
	require.NoError(t, err)
	kid, err := jwkThumbprint(x, y)
	require.NoError(t, err)
	assert.Equal(t, kid, key.KID)
	assert.Equal(t, key.KID, key.KeyID())
}

// A stored key survives a round trip through its persisted encoding.
func TestSigningKeyReloadsFromItsScalar(t *testing.T) {
	encoded, err := newEncodedKey()
	require.NoError(t, err)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	first, err := keyFromScalar(raw)
	require.NoError(t, err)
	again, err := keyFromScalar(raw)
	require.NoError(t, err)
	assert.Equal(t, first.JWK(), again.JWK())

	_, err = keyFromScalar([]byte("short"))
	assert.Error(t, err)
}
