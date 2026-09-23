package httpsig

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ed25519Signer adapts the RFC's test key; Ed25519 is deterministic, so
// the signature must match the published vector byte for byte.
type ed25519Signer struct {
	key ed25519.PrivateKey
	kid string
}

func (s ed25519Signer) KeyID() string { return s.kid }

func (s ed25519Signer) Sign(base []byte) ([]byte, error) {
	return ed25519.Sign(s.key, base), nil
}

// rfcTestRequest is the RFC 9421 appendix B.2 test-request message.
func rfcTestRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		"https://example.com/foo?param=Value&Pet=dog",
		strings.NewReader(`{"hello": "world"}`))
	require.NoError(t, err)
	req.Header.Set("Date", "Tue, 20 Apr 2021 02:07:55 GMT")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Digest", "sha-512=:WZDPaVn/7XgHaAy8pmojAkGWoRx"+
		"2UFChF41A2svX+TaPm+AbwAgBWnrIiYllu7BNNyealdVLvRwEmTHWXvJwew==:")
	req.Header.Set("Content-Length", "18")
	return req
}

// RFC 9421 appendix B.2.6: signing the test request with
// test-key-ed25519 (appendix B.1.4).
func TestSignMatchesRFC9421Ed25519Vector(t *testing.T) {
	seed, err := base64.RawURLEncoding.DecodeString(
		"n4Ni-HpISpVObnQMW0wOhCKROaIKqKtW_2ZYb2p9KcU")
	require.NoError(t, err)
	signer := ed25519Signer{
		key: ed25519.NewKeyFromSeed(seed), kid: "test-key-ed25519",
	}
	req := rfcTestRequest(t)
	components := []string{"date", "@method", "@path", "@authority",
		"content-type", "content-length"}

	params := signatureParams(components, Params{Created: 1618884473},
		signer.KeyID())
	base, err := signatureBase(req, components, params)
	require.NoError(t, err)
	assert.Equal(t, `"date": Tue, 20 Apr 2021 02:07:55 GMT
"@method": POST
"@path": /foo
"@authority": example.com
"content-type": application/json
"content-length": 18
"@signature-params": ("date" "@method" "@path" "@authority" `+
		`"content-type" "content-length");created=1618884473`+
		`;keyid="test-key-ed25519"`, base)

	require.NoError(t, Sign(req, "sig-b26", components,
		Params{Created: 1618884473}, signer))
	assert.Equal(t, `sig-b26=("date" "@method" "@path" "@authority" `+
		`"content-type" "content-length");created=1618884473`+
		`;keyid="test-key-ed25519"`, req.Header.Get("Signature-Input"))
	assert.Equal(t, "sig-b26=:wqcAqbmYJ2ji2glfAMaRy4gruYYnx2nEFN2HN6jrnDnQ"+
		"CK1u02Gb04v9EDgwUPiu4A0w6vuQv5lIp5WPpBKRCw==:",
		req.Header.Get("Signature"))
}

func TestDerivedComponents(t *testing.T) {
	req := rfcTestRequest(t)
	for component, want := range map[string]string{
		"@query":     "?param=Value&Pet=dog",
		"@authority": "example.com",
	} {
		got, err := componentValue(req, component)
		require.NoError(t, err)
		assert.Equal(t, want, got, component)
	}

	// A non-default port is part of the authority; the default is not.
	for raw, want := range map[string]string{
		"https://Platform.Example:443/x": "platform.example",
		"https://platform.example:8443/": "platform.example:8443",
		"http://127.0.0.1:80":            "127.0.0.1",
	} {
		r, err := http.NewRequest(http.MethodPost, raw, nil)
		require.NoError(t, err)
		assert.Equal(t, want, authority(r), raw)
	}

	r, err := http.NewRequest(http.MethodPost, "https://platform.example", nil)
	require.NoError(t, err)
	path, err := componentValue(r, "@path")
	require.NoError(t, err)
	assert.Equal(t, "/", path, "an empty path signs as /")
}

func TestSignRefusesWhatItCannotCover(t *testing.T) {
	req := rfcTestRequest(t)
	signer := ed25519Signer{key: ed25519.NewKeyFromSeed(make([]byte, 32))}
	assert.ErrorContains(t,
		Sign(req, "sig1", []string{"ucp-agent"}, Params{}, signer),
		`"ucp-agent" is not set`)
	assert.ErrorContains(t,
		Sign(req, "sig1", []string{"@target-uri"}, Params{}, signer),
		"unsupported component")
}

// RFC 9530 appendix B.1: the sha-256 digest of {"hello": "world"}.
func TestContentDigest(t *testing.T) {
	assert.Equal(t,
		"sha-256=:X48E9qOokqqrvdts8nOJRJN3OWDUoyWxBf7kbu9DBPE=:",
		ContentDigest([]byte(`{"hello": "world"}`)))
}
