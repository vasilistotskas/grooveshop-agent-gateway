package ucp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"math/big"
	"strings"
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

// The key id is the RFC 7638 thumbprint, recomputable from the JWK. The
// thumbprint is checked against RFC 8037 appendix A.3.
func TestSigningKeyIDIsTheJWKThumbprint(t *testing.T) {
	assert.Equal(t, "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k",
		thumbprint(map[string]string{
			"crv": "Ed25519", "kty": "OKP",
			"x": "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo",
		}))

	key := testKey(t)
	jwk := key.JWK()
	assert.Equal(t, thumbprint(map[string]string{
		"crv": jwk["crv"], "kty": jwk["kty"], "x": jwk["x"], "y": jwk["y"],
	}), key.KID)
	assert.Equal(t, key.KID, key.KeyID())
}

// A stored key survives a round trip through its scalar.
func TestSigningKeyReloadsFromItsScalar(t *testing.T) {
	raw, err := newScalar()
	require.NoError(t, err)
	first, err := SigningKeyFromScalar(raw)
	require.NoError(t, err)
	again, err := SigningKeyFromScalar(raw)
	require.NoError(t, err)
	assert.Equal(t, first.JWK(), again.JWK())

	_, err = SigningKeyFromScalar([]byte("short"))
	assert.Error(t, err)
}

// testKEK is a fresh key-encryption key.
func testKEK(t *testing.T) []byte {
	t.Helper()
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	return kek
}

// sealingKeys is a Keys for its sealing alone: no Redis.
func sealingKeys(t *testing.T, kek []byte) *Keys {
	t.Helper()
	k, err := NewKeys(nil, kek)
	require.NoError(t, err)
	return k
}

// A store's key is stored sealed: what Redis holds carries neither the
// scalar nor its base64, and opens back to the scalar.
func TestSigningKeyIsStoredSealed(t *testing.T) {
	keys := sealingKeys(t, testKEK(t))
	scalar, err := newScalar()
	require.NoError(t, err)

	stored := keys.seal("alpha", scalar)
	assert.NotContains(t, stored, base64.StdEncoding.EncodeToString(scalar))
	raw, err := base64.StdEncoding.DecodeString(stored)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), string(scalar))
	// A random 96-bit nonce and a 128-bit tag around the scalar.
	assert.Len(t, raw, len(scalar)+12+16)
	assert.NotEqual(t, stored, keys.seal("alpha", scalar),
		"each seal draws its own nonce")

	opened, err := keys.open("alpha", stored)
	require.NoError(t, err)
	assert.Equal(t, scalar, opened)
}

// A sealed key opens only for the store it was sealed for, only with
// the key-encryption key that sealed it, and only as it was written.
func TestSealedSigningKeyOpensOnlyWhereItWasSealed(t *testing.T) {
	kek := testKEK(t)
	keys := sealingKeys(t, kek)
	scalar, err := newScalar()
	require.NoError(t, err)
	stored := keys.seal("alpha", scalar)

	_, err = keys.open("beta", stored)
	require.Error(t, err, "a record moved to another store must not open")
	_, err = keys.open("alph", stored)
	require.Error(t, err)

	_, err = sealingKeys(t, testKEK(t)).open("alpha", stored)
	require.Error(t, err, "another key-encryption key must not open it")

	raw, err := base64.StdEncoding.DecodeString(stored)
	require.NoError(t, err)
	raw[len(raw)/2] ^= 1
	_, err = keys.open("alpha", base64.StdEncoding.EncodeToString(raw))
	require.Error(t, err, "a changed record must not open")

	_, err = keys.open("alpha", "not base64!")
	require.Error(t, err)
	_, err = keys.open("alpha", base64.StdEncoding.EncodeToString(scalar))
	require.Error(t, err, "an unsealed scalar is not a sealed record")

	// The refusal names no key material.
	_, err = keys.open("beta", stored)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), stored)
}

// Only an AES-256 key seals: a shorter one would boot with a weaker
// cipher than the configuration promises.
func TestNewKeysRefusesAKeyEncryptionKeyThatIsNotAES256(t *testing.T) {
	for _, n := range []int{0, 16, 24, 31, 33} {
		_, err := NewKeys(nil, make([]byte, n))
		require.Error(t, err, "%d bytes", n)
		assert.True(t, strings.HasPrefix(err.Error(), "ucp: "))
	}
}
