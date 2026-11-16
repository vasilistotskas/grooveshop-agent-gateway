package httpsig

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dunglas/httpsfv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ecSigner signs as a platform does: ECDSA, fixed-width r||s.
type ecSigner struct {
	key *ecdsa.PrivateKey
	kid string
}

func (s ecSigner) KeyID() string { return s.kid }

func (s ecSigner) Sign(base []byte) ([]byte, error) {
	var digest []byte
	if s.key.Curve == elliptic.P384() {
		sum := sha512.Sum384(base)
		digest = sum[:]
	} else {
		sum := sha256.Sum256(base)
		digest = sum[:]
	}
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, digest)
	if err != nil {
		return nil, err
	}
	size := (s.key.Curve.Params().BitSize + 7) / 8
	sig := make([]byte, 2*size)
	r.FillBytes(sig[:size])
	sv.FillBytes(sig[size:])
	return sig, nil
}

func newECSigner(t *testing.T, curve elliptic.Curve, kid string) ecSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	require.NoError(t, err)
	return ecSigner{key: key, kid: kid}
}

// keysOf serves each key under its kid; any other kid is unknown.
func keysOf(keys map[string]Key) KeySource {
	return func(_ context.Context, kid string) (Key, error) {
		k, ok := keys[kid]
		if !ok {
			return Key{}, ErrUnknownKey
		}
		return k, nil
	}
}

var (
	testNow    = time.Unix(1_800_000_000, 0)
	testWindow = Window{MaxAge: 5 * time.Minute, Skew: time.Minute}
	mcpBody    = []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call",` +
		`"params":{"name":"get_order","arguments":{"id":"o1"}}}`)
	ucpComponents = []string{"@method", "@authority", "@path",
		"content-digest", "content-type", "ucp-agent"}
)

// received is a platform's POST to /mcp as the gateway's server sees it:
// the target in Host, a URL with no host or scheme.
func received(body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(string(body)))
	req.Host = "shop.example"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("UCP-Agent",
		`profile="https://platform.example/.well-known/ucp"`)
	req.Header.Set("Content-Digest", ContentDigest(body))
	return req
}

func signed(t *testing.T, s Signer, components []string, p Params) *http.Request {
	t.Helper()
	req := received(mcpBody)
	require.NoError(t, Sign(req, "sig1", components, p, s))
	return req
}

func verify(req *http.Request, body []byte, keys map[string]Key) (string, error) {
	return Verify(context.Background(), req, body, keysOf(keys), testWindow,
		testNow)
}

func refusalCode(t *testing.T, err error) string {
	t.Helper()
	e, ok := errors.AsType[*Error](err)
	require.True(t, ok, "%v", err)
	return e.Code
}

func TestVerifyAcceptsWhatSignSigns(t *testing.T) {
	for name, curve := range map[string]elliptic.Curve{
		"ES256": elliptic.P256(), "ES384": elliptic.P384(),
	} {
		t.Run(name, func(t *testing.T) {
			s := newECSigner(t, curve, "k1")
			req := signed(t, s, ucpComponents,
				Params{Created: testNow.Unix()})
			label, err := verify(req, mcpBody,
				map[string]Key{"k1": {Public: &s.key.PublicKey}})
			require.NoError(t, err)
			assert.Equal(t, "sig1", label)
		})
	}
	t.Run("Ed25519", func(t *testing.T) {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		req := signed(t, ed25519Signer{key: priv, kid: "k1"},
			ucpComponents, Params{})
		_, err = verify(req, mcpBody, map[string]Key{"k1": {Public: pub}})
		require.NoError(t, err)
	})
}

// RFC 9421 appendix B.2.6, verified: the base rebuilt from the received
// Signature-Input, and the published signature over it.
func TestVerifyBaseMatchesRFC9421Ed25519Vector(t *testing.T) {
	req := rfcTestRequest(t)
	req.Header.Set("Signature-Input", `sig-b26=("date" "@method" "@path" `+
		`"@authority" "content-type" "content-length");`+
		`created=1618884473;keyid="test-key-ed25519"`)
	inputs, err := httpsfv.UnmarshalDictionary(
		req.Header.Values("Signature-Input"))
	require.NoError(t, err)
	m, _ := inputs.Get("sig-b26")
	base, err := verifyBase(req, m.(httpsfv.InnerList))
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

	pub, err := base64.RawURLEncoding.DecodeString(
		"JrQLj5P_89iXES9-vFgrIy29clF9CC_oPPsw3c5D0bs")
	require.NoError(t, err)
	sig, err := base64.StdEncoding.DecodeString("wqcAqbmYJ2ji2glfAMaRy4gru" +
		"YYnx2nEFN2HN6jrnDnQCK1u02Gb04v9EDgwUPiu4A0w6vuQv5lIp5WPpBKRCw==")
	require.NoError(t, err)
	require.NoError(t, verifyWith(ed25519.PublicKey(pub), []byte(base), sig,
		"sig-b26"))
}

func TestVerifyRefuses(t *testing.T) {
	s := newECSigner(t, elliptic.P256(), "k1")
	keys := map[string]Key{"k1": {Public: &s.key.PublicKey}}
	other := newECSigner(t, elliptic.P256(), "k1")
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		req  func(t *testing.T) *http.Request
		body []byte
		keys map[string]Key
		code string
	}{
		"a body other than the one digested": {
			req:  func(t *testing.T) *http.Request { return signed(t, s, ucpComponents, Params{}) },
			body: []byte(`{"jsonrpc":"2.0","id":7}`),
			code: CodeDigestMismatch,
		},
		"a covered header changed after signing": {
			req: func(t *testing.T) *http.Request {
				req := signed(t, s, ucpComponents, Params{})
				req.Header.Set("UCP-Agent", `profile="https://other.example/p"`)
				return req
			},
			code: CodeSignatureInvalid,
		},
		"another key under the same kid": {
			req:  func(t *testing.T) *http.Request { return signed(t, other, ucpComponents, Params{}) },
			code: CodeSignatureInvalid,
		},
		"a kid the signer does not publish": {
			req:  func(t *testing.T) *http.Request { return signed(t, s, ucpComponents, Params{}) },
			keys: map[string]Key{"k2": {Public: &s.key.PublicKey}},
			code: CodeKeyNotFound,
		},
		"a key of a type this verifier does not use": {
			req:  func(t *testing.T) *http.Request { return signed(t, s, ucpComponents, Params{}) },
			keys: map[string]Key{"k1": {Public: &rsaKey.PublicKey}},
			code: CodeAlgorithmUnsupported,
		},
		"ucp-agent not covered": {
			req: func(t *testing.T) *http.Request {
				return signed(t, s, ucpComponents[:5], Params{})
			},
			code: CodeSignatureInvalid,
		},
		"content-digest not covered": {
			req: func(t *testing.T) *http.Request {
				return signed(t, s, []string{"@method", "@authority",
					"@path", "content-type", "ucp-agent"}, Params{})
			},
			code: CodeSignatureInvalid,
		},
		"@authority not covered": {
			req: func(t *testing.T) *http.Request {
				return signed(t, s, []string{"@method", "@path",
					"content-digest", "content-type", "ucp-agent"}, Params{})
			},
			code: CodeSignatureInvalid,
		},
		"an idempotency key sent and not covered": {
			req: func(t *testing.T) *http.Request {
				req := received(mcpBody)
				req.Header.Set("Idempotency-Key", "k-1")
				require.NoError(t, Sign(req, "sig1", ucpComponents,
					Params{}, s))
				return req
			},
			code: CodeSignatureInvalid,
		},
		"a DER-encoded ECDSA signature": {
			req: func(t *testing.T) *http.Request {
				req := signed(t, s, ucpComponents, Params{})
				der, err := ecdsa.SignASN1(rand.Reader, s.key,
					make([]byte, 32))
				require.NoError(t, err)
				req.Header.Set("Signature", "sig1=:"+
					base64.StdEncoding.EncodeToString(der)+":")
				return req
			},
			code: CodeSignatureInvalid,
		},
		"an alg parameter": {
			req: func(t *testing.T) *http.Request {
				req := signed(t, s, ucpComponents, Params{})
				req.Header.Set("Signature-Input", req.Header.Get(
					"Signature-Input")+`;alg="ecdsa-p256-sha256"`)
				return req
			},
			code: CodeSignatureInvalid,
		},
		"created before the window": {
			req: func(t *testing.T) *http.Request {
				return signed(t, s, ucpComponents, Params{
					Created: testNow.Add(-testWindow.MaxAge - time.Second).Unix(),
				})
			},
			code: CodeSignatureInvalid,
		},
		"created ahead of the clock beyond the skew": {
			req: func(t *testing.T) *http.Request {
				return signed(t, s, ucpComponents, Params{
					Created: testNow.Add(testWindow.Skew + time.Second).Unix(),
				})
			},
			code: CodeSignatureInvalid,
		},
		"an expires that has passed": {
			req: func(t *testing.T) *http.Request {
				req := signed(t, s, ucpComponents, Params{})
				expired := testNow.Add(-testWindow.Skew - time.Second)
				req.Header.Set("Signature-Input", req.Header.Get(
					"Signature-Input")+";expires="+
					strconv.FormatInt(expired.Unix(), 10))
				return req
			},
			code: CodeSignatureInvalid,
		},
		"a tag other than web-bot-auth": {
			req: func(t *testing.T) *http.Request {
				req := signed(t, s, ucpComponents, Params{})
				req.Header.Set("Signature-Input", req.Header.Get(
					"Signature-Input")+`;tag="other"`)
				return req
			},
			code: CodeSignatureInvalid,
		},
		"a web-bot-auth signature whose keyid is not its thumbprint": {
			req: func(t *testing.T) *http.Request {
				req := signed(t, s, ucpComponents, Params{})
				req.Header.Set("Signature-Input", req.Header.Get(
					"Signature-Input")+`;tag="web-bot-auth"`)
				return req
			},
			code: CodeSignatureInvalid,
		},
		"a Signature-Input that is not a dictionary": {
			req: func(t *testing.T) *http.Request {
				req := signed(t, s, ucpComponents, Params{})
				req.Header.Set("Signature-Input", "sig1=(")
				return req
			},
			code: CodeSignatureInvalid,
		},
		"no signature under the input's label": {
			req: func(t *testing.T) *http.Request {
				req := signed(t, s, ucpComponents, Params{})
				req.Header.Set("Signature", strings.Replace(
					req.Header.Get("Signature"), "sig1=", "sig2=", 1))
				return req
			},
			code: CodeSignatureInvalid,
		},
		"a component named twice": {
			req: func(t *testing.T) *http.Request {
				return signed(t, s, append([]string{"@method"},
					ucpComponents...), Params{})
			},
			code: CodeSignatureInvalid,
		},
	} {
		t.Run(name, func(t *testing.T) {
			body := tc.body
			if body == nil {
				body = mcpBody
			}
			use := tc.keys
			if use == nil {
				use = keys
			}
			_, err := verify(tc.req(t), body, use)
			require.Error(t, err)
			assert.Equal(t, tc.code, refusalCode(t, err))
		})
	}
}

// The window bounds only what a signature carries: one without created
// or expires is not refused for them.
func TestVerifyTakesASignatureWithinItsWindow(t *testing.T) {
	s := newECSigner(t, elliptic.P256(), "k1")
	keys := map[string]Key{"k1": {Public: &s.key.PublicKey}}
	for name, created := range map[string]int64{
		"no created":              0,
		"created now":             testNow.Unix(),
		"created at the max age":  testNow.Add(-testWindow.MaxAge).Unix(),
		"created within the skew": testNow.Add(testWindow.Skew).Unix(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := verify(signed(t, s, ucpComponents,
				Params{Created: created}), mcpBody, keys)
			require.NoError(t, err)
		})
	}
}

// A request may carry several signatures: one that verifies is enough,
// in any order, and the fields may arrive over several header lines.
func TestVerifyTakesAnyOneSignatureThatVerifies(t *testing.T) {
	good := newECSigner(t, elliptic.P256(), "good")
	bad := newECSigner(t, elliptic.P256(), "gone")
	keys := map[string]Key{"good": {Public: &good.key.PublicKey}}

	req := received(mcpBody)
	require.NoError(t, Sign(req, "first", ucpComponents, Params{}, bad))
	firstInput, firstSig := req.Header.Get("Signature-Input"),
		req.Header.Get("Signature")
	require.NoError(t, Sign(req, "second", ucpComponents, Params{}, good))
	req.Header.Add("Signature-Input", firstInput)
	req.Header.Add("Signature", firstSig)

	label, err := verify(req, mcpBody, keys)
	require.NoError(t, err)
	assert.Equal(t, "second", label)

	// None verifies: the first one's refusal is the answer.
	_, err = verify(req, mcpBody, map[string]Key{})
	assert.Equal(t, CodeKeyNotFound, refusalCode(t, err))
}

// The authority a request was signed for is its Host, with a default
// port dropped whichever side wrote it.
func TestVerifyReadsTheAuthorityFromHost(t *testing.T) {
	s := newECSigner(t, elliptic.P256(), "k1")
	keys := map[string]Key{"k1": {Public: &s.key.PublicKey}}

	req := signed(t, s, ucpComponents, Params{})
	req.Host = "Shop.Example:443"
	_, err := verify(req, mcpBody, keys)
	require.NoError(t, err, "the default port is not the authority's")

	req = signed(t, s, ucpComponents, Params{})
	req.Host = "other.example"
	_, err = verify(req, mcpBody, keys)
	assert.Equal(t, CodeSignatureInvalid, refusalCode(t, err),
		"a signature for one store is not one for another")
}

// The web-bot-auth shape covers signature-agent by its dictionary member
// (RFC 9421 section 2.1.2), and names its key by the key's thumbprint.
func TestVerifyTakesTheWebBotAuthShape(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	const thumb = "thumbprint-of-the-key"
	req := received(mcpBody)
	req.Header.Set("Signature-Agent",
		`sig1="https://platform.example/.well-known/ucp";type=jwks_uri`)
	params := `;created=` + strconv.FormatInt(testNow.Unix(), 10) +
		`;expires=` + strconv.FormatInt(testNow.Add(time.Hour).Unix(), 10) +
		`;keyid="` + thumb + `";tag="web-bot-auth"`
	input := `("@method" "@authority" "@path" "signature-agent";key="sig1" ` +
		`"ucp-agent" "content-digest" "content-type")` + params
	req.Header.Set("Signature-Input", "sig1="+input)
	list, err := httpsfv.UnmarshalDictionary([]string{"sig1=" + input})
	require.NoError(t, err)
	m, _ := list.Get("sig1")
	base, err := verifyBase(req, m.(httpsfv.InnerList))
	require.NoError(t, err)
	assert.Contains(t, base, `"signature-agent";key="sig1": `+
		`"https://platform.example/.well-known/ucp";type=jwks_uri`+"\n")
	req.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(
		ed25519.Sign(priv, []byte(base)))+":")

	_, err = verify(req, mcpBody,
		map[string]Key{thumb: {Public: pub, Thumbprint: thumb}})
	require.NoError(t, err)

	req.Header.Del("Signature-Agent")
	_, err = verify(req, mcpBody,
		map[string]Key{thumb: {Public: pub, Thumbprint: thumb}})
	assert.Equal(t, CodeSignatureInvalid, refusalCode(t, err),
		"a covered dictionary member that is not sent")
}

// A key source's own failure is not a refusal: it ends the verification
// with that error, for the caller to answer as an outage.
func TestVerifyPassesOnAKeySourceFailure(t *testing.T) {
	s := newECSigner(t, elliptic.P256(), "k1")
	outage := errors.New("profile fetch: busy")
	_, err := Verify(t.Context(),
		signed(t, s, ucpComponents, Params{}), mcpBody,
		func(context.Context, string) (Key, error) { return Key{}, outage },
		testWindow, testNow)
	assert.ErrorIs(t, err, outage)
}
