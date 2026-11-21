package ucp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpsig"
)

// withKeys is a platform profile publishing keys in its keys[].
func withKeys(t *testing.T, profile string, keys ...map[string]string) string {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(profile), &doc))
	doc["keys"] = keys
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	return string(out)
}

func testSigningKey(t *testing.T) *SigningKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	raw, err := priv.Bytes()
	require.NoError(t, err)
	key, err := SigningKeyFromScalar(raw)
	require.NoError(t, err)
	return key
}

func TestJWKVerifyKey(t *testing.T) {
	es := testSigningKey(t).JWK()
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	point, err := p384.PublicKey.Bytes()
	require.NoError(t, err)
	b64 := base64.RawURLEncoding.EncodeToString

	for name, tc := range map[string]struct {
		jwk  JWK
		want error
	}{
		"ES256": {jwk: JWK{Kid: "a", Kty: "EC", Crv: "P-256",
			X: es["x"], Y: es["y"]}},
		"ES384": {jwk: JWK{Kid: "a", Kty: "EC", Crv: "P-384",
			X: b64(point[1:49]), Y: b64(point[49:])}},
		// The OKP example of UCP's signatures.md (Key Format): its kid
		// is its RFC 7638 thumbprint.
		"Ed25519": {jwk: JWK{Kid: "poqkLGiymh_W0uP6PZFw-dvez3QJT5SolqXBCW38r0U",
			Kty: "OKP", Crv: "Ed25519",
			X: "JrQLj5P_89iXES9-vFgrIy29clF9CC_oPPsw3c5D0bs"}},
		"an RSA key": {jwk: JWK{Kid: "a", Kty: "RSA"},
			want: httpsig.ErrUnsupportedKey},
		"a curve this verifier does not use": {jwk: JWK{Kid: "a",
			Kty: "EC", Crv: "P-521", X: es["x"], Y: es["y"]},
			want: httpsig.ErrUnsupportedKey},
		"a point off the curve": {jwk: JWK{Kid: "a", Kty: "EC",
			Crv: "P-256", X: es["x"], Y: es["x"]},
			want: httpsig.ErrUnsupportedKey},
		"coordinates of the wrong length": {jwk: JWK{Kid: "a", Kty: "EC",
			Crv: "P-256", X: es["x"][:20], Y: es["y"]},
			want: httpsig.ErrUnsupportedKey},
		"an Ed25519 key of the wrong length": {jwk: JWK{Kid: "a",
			Kty: "OKP", Crv: "Ed25519", X: es["x"][:20]},
			want: httpsig.ErrUnsupportedKey},
	} {
		t.Run(name, func(t *testing.T) {
			key, err := tc.jwk.verifyKey()
			if tc.want != nil {
				assert.ErrorIs(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, key.Public)
		})
	}

	okp, err := JWK{Kty: "OKP", Crv: "Ed25519",
		X: "JrQLj5P_89iXES9-vFgrIy29clF9CC_oPPsw3c5D0bs"}.verifyKey()
	require.NoError(t, err)
	assert.Equal(t, "poqkLGiymh_W0uP6PZFw-dvez3QJT5SolqXBCW38r0U",
		okp.Thumbprint)
	ec, err := JWK{Kty: "EC", Crv: "P-256", X: es["x"], Y: es["y"]}.verifyKey()
	require.NoError(t, err)
	assert.Equal(t, es["kid"], ec.Thumbprint)
}

// Keys marked for another use are skipped when a kid is looked up
// (RFC 7517 sections 4.2 and 4.3).
func TestProfileKeySkipsKeysNotForVerifying(t *testing.T) {
	p := &PlatformProfile{Keys: []json.RawMessage{
		json.RawMessage(`{"kid":"enc","kty":"EC","use":"enc"}`),
		json.RawMessage(`{"kid":"ops","kty":"EC","key_ops":["sign"]}`),
		json.RawMessage(`{"kid":"opstr","kty":"EC","key_ops":"verify"}`),
		json.RawMessage(`{"kid":"odd","kty":"EC","x":1}`),
		json.RawMessage(`{"kid":"sig","kty":"EC","use":"sig","key_ops":["verify"]}`),
		json.RawMessage(`{"kid":"plain","kty":"EC"}`),
	}}
	for kid, want := range map[string]bool{
		"enc": false, "ops": false, "opstr": false, "odd": false,
		"sig": true, "plain": true, "none": false,
	} {
		_, ok := p.key(kid)
		assert.Equal(t, want, ok, kid)
	}
}

// A key this business cannot read is skipped, never a profile refused:
// negotiation goes on with the profile, and its other keys verify.
func TestAKeyThatDoesNotDecodeLeavesTheProfileUsable(t *testing.T) {
	key := testSigningKey(t)
	body := withKeys(t, platformProfileJSON(t, "https://p.example/h"),
		map[string]string{"kid": "odd", "kty": "RSA", "key_ops": "verify"},
		key.JWK())
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	r := newTestResolver(t)

	p, err := r.Resolve(caller(t), srv.URL+"/profile.json")
	require.NoError(t, err)
	assert.True(t, Negotiate(BusinessCapabilities(testTenant()), p).
		Has(CapabilityCheckout))
	req, raw := signedRequest(t, key, srv.URL+"/profile.json")
	_, err = r.VerifyRequest(caller(t), req, raw, testWindow, time.Now())
	require.NoError(t, err)
}

func TestNormalizeProfileURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://Platform.Example/.well-known/ucp":     "https://platform.example/.well-known/ucp",
		"HTTPS://platform.example:443/.well-known/ucp": "https://platform.example/.well-known/ucp",
		"https://platform.example:8443/p":              "https://platform.example:8443/p",
		"http://127.0.0.1:80/p":                        "http://127.0.0.1/p",
		"https://platform.example":                     "https://platform.example/",
		"https://platform.example/P?x=1#frag":          "https://platform.example/P?x=1",
		"https://bücher.example/ucp":                   "https://xn--bcher-kva.example/ucp",
		"https://[2001:DB8::1]:443/ucp":                "https://[2001:db8::1]/ucp",
		"https://platform.example/a%2Fb":               "https://platform.example/a%2Fb",
		"https://platform.example/.well-known/ucp?":    "https://platform.example/.well-known/ucp?",
	} {
		got, err := NormalizeProfileURL(raw)
		require.NoError(t, err, raw)
		assert.Equal(t, want, got, raw)
	}
	for _, raw := range []string{
		"", "platform.example/ucp", "ftp://platform.example/ucp",
		"https://user@platform.example/ucp", "https:///ucp",
	} {
		_, err := NormalizeProfileURL(raw)
		assert.ErrorIs(t, err, ErrPlatformURL, raw)
	}
}

func TestUCPAgentProfile(t *testing.T) {
	got, err := ucpAgentProfile(
		[]string{`profile="https://platform.example/.well-known/ucp"`})
	require.NoError(t, err)
	assert.Equal(t, "https://platform.example/.well-known/ucp", got)

	for _, values := range [][]string{
		nil,
		{`profile=https`},
		{`profile=("https://a.example/p")`},
		{`other="https://a.example/p"`},
		{`profile=""`},
		{`profile="https://a.example/p`},
	} {
		_, err := ucpAgentProfile(values)
		assert.ErrorIs(t, err, errNoUCPAgent, "%q", values)
	}
}

// The spec allows one forced refresh per origin per TTL floor.
func TestRefreshIsOncePerOriginPerTTLFloor(t *testing.T) {
	body := platformProfileJSON(t, "https://p.example/hooks")
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	r := newTestResolver(t)
	ctx := caller(t)

	_, err := r.Resolve(ctx, srv.URL+"/a.json")
	require.NoError(t, err)
	_, err = r.Refresh(ctx, srv.URL+"/a.json")
	require.NoError(t, err)
	assert.EqualValues(t, 2, srv.hits.Load(), "refetched though cached")

	_, err = r.Refresh(ctx, srv.URL+"/b.json")
	require.ErrorIs(t, err, errRefreshSpent, "same origin, other path")
	assert.EqualValues(t, 2, srv.hits.Load())

	r.mu.Lock()
	for o := range r.refreshed {
		r.refreshed[o] = time.Now().Add(-profileTTLFloor)
	}
	r.mu.Unlock()
	_, err = r.Refresh(ctx, srv.URL+"/a.json")
	require.NoError(t, err)
	assert.EqualValues(t, 3, srv.hits.Load())
}

// signedRequest is a platform's POST to a store's /mcp, signed with key
// under profileURL as the platform signs it.
func signedRequest(
	t *testing.T, key *SigningKey, profileURL string,
) (*http.Request, []byte) {
	t.Helper()
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(string(body)))
	req.Host = "shop.example"
	req.Header.Set("Content-Type", "application/json")
	require.NoError(t, SignRequest(req, body, profileURL, key, time.Now()))
	return req, body
}

var testWindow = httpsig.Window{MaxAge: time.Minute, Skew: time.Minute}

func TestVerifyRequestNamesTheSigner(t *testing.T) {
	key := testSigningKey(t)
	var body atomic.Value
	body.Store(withKeys(t, platformProfileJSON(t, "https://p.example/h"),
		key.JWK()))
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body.Load().(string)))
	})
	r := newTestResolver(t)
	ctx := caller(t)
	profileURL := strings.Replace(srv.URL, "http://", "HTTP://", 1) +
		"/profile.json"

	req, raw := signedRequest(t, key, profileURL)
	signer, err := r.VerifyRequest(ctx, req, raw, testWindow, time.Now())
	require.NoError(t, err)
	assert.Equal(t, srv.URL+"/profile.json", signer,
		"the signer is its profile URL, normalized")

	// A key the cached profile does not publish yet: the profile is
	// fetched again, once, and the rotated key verifies.
	rotated := testSigningKey(t)
	body.Store(withKeys(t, platformProfileJSON(t, "https://p.example/h"),
		key.JWK(), rotated.JWK()))
	req, raw = signedRequest(t, rotated, profileURL)
	_, err = r.VerifyRequest(ctx, req, raw, testWindow, time.Now())
	require.NoError(t, err)
	hits := srv.hits.Load()

	// Another unknown key within the floor: no fetch, key_not_found.
	req, raw = signedRequest(t, testSigningKey(t), profileURL)
	_, err = r.VerifyRequest(ctx, req, raw, testWindow, time.Now())
	sigErr, ok := errors.AsType[*httpsig.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, httpsig.CodeKeyNotFound, sigErr.Code)
	assert.Equal(t, hits, srv.hits.Load())
}

func TestVerifyRequestRefusesWhatNamesNoSigner(t *testing.T) {
	key := testSigningKey(t)
	r := newTestResolver(t)
	ctx := caller(t)

	req, raw := signedRequest(t, key, "https://platform.example/ucp")
	req.Header.Del("UCP-Agent")
	_, err := r.VerifyRequest(ctx, req, raw, testWindow, time.Now())
	require.ErrorIs(t, err, errNoUCPAgent)

	// A profile that cannot be had is the discovery failure.
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	req, raw = signedRequest(t, key, srv.URL+"/profile.json")
	_, err = r.VerifyRequest(ctx, req, raw, testWindow, time.Now())
	assert.Equal(t, CodeProfileUnreachable, discoveryCode(t, err))
}

// A refresh replaces the cached profile only with one it fetched: one
// that fails keeps the cached profile, and one that never reached the
// origin (the discovery budget spent) does not count against its floor.
func TestRefreshKeepsTheCachedProfileUntilItHasAnother(t *testing.T) {
	good := platformProfileJSON(t, "https://p.example/h")
	var broken atomic.Bool
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		if broken.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(good))
	})
	r := newTestResolver(t)
	url := srv.URL + "/profile.json"
	_, err := r.Resolve(caller(t), url)
	require.NoError(t, err)

	r.limiter = rate.NewLimiter(0, 0)
	_, err = r.Refresh(caller(t), url)
	require.ErrorIs(t, err, ErrDiscoveryBusy)
	r.limiter = rate.NewLimiter(rate.Inf, 1)
	_, err = r.Resolve(caller(t), url)
	require.NoError(t, err, "a busy refresh keeps the cached profile")

	broken.Store(true)
	_, err = r.Refresh(caller(t), url)
	assert.Equal(t, CodeProfileUnreachable, discoveryCode(t, err),
		"the busy refresh did not spend the origin's floor")
	_, err = r.Resolve(caller(t), url)
	require.NoError(t, err, "a failed refresh keeps the cached profile")
	_, err = r.Refresh(caller(t), url)
	require.ErrorIs(t, err, errRefreshSpent)
}

// The refresh floor is the origin's, however its URL spells it.
func TestRefreshFloorIsTheNormalizedOrigin(t *testing.T) {
	r := newTestResolver(t)
	r.refreshed["https://platform.example"] = time.Now()
	for _, raw := range []string{
		"https://platform.example:443/ucp",
		"HTTPS://Platform.Example/other",
	} {
		_, err := r.Refresh(caller(t), raw)
		require.ErrorIs(t, err, errRefreshSpent, raw)
	}
}

// A key the cached profile does not publish, on a profile that can no
// longer be fetched, is the discovery failure, not a missing key.
func TestVerifyRequestAnswersAProfileThatCannotBeRefetched(t *testing.T) {
	key := testSigningKey(t)
	var broken atomic.Bool
	body := withKeys(t, platformProfileJSON(t, "https://p.example/h"),
		key.JWK())
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		if broken.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	r := newTestResolver(t)
	_, err := r.Resolve(caller(t), srv.URL+"/profile.json")
	require.NoError(t, err)

	broken.Store(true)
	req, raw := signedRequest(t, testSigningKey(t), srv.URL+"/profile.json")
	_, err = r.VerifyRequest(caller(t), req, raw, testWindow, time.Now())
	assert.Equal(t, CodeProfileUnreachable, discoveryCode(t, err))
}

// SignRequest covers what the verifier requires of the request it signs:
// an empty query, a Signature-Agent, an Idempotency-Key; and it writes
// the profile URL as a structured-field string.
func TestSignRequestCoversWhatTheRequestCarries(t *testing.T) {
	key := testSigningKey(t)
	body := withKeys(t, platformProfileJSON(t, "https://p.example/h"),
		key.JWK())
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	r := newTestResolver(t)
	profile := srv.URL + `/profile.json?name="a\b"`

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp?",
		strings.NewReader(string(raw)))
	req.Host = "shop.example"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "k-1")
	req.Header.Set("Signature-Agent",
		`sig1="https://platform.example/.well-known/ucp";type=jwks_uri`)
	require.NoError(t, SignRequest(req, raw, profile, key, time.Now()))
	assert.Equal(t, `profile="`+srv.URL+`/profile.json?name=\"a\\b\""`,
		req.Header.Get("UCP-Agent"))
	for _, c := range []string{`"@query"`, `"signature-agent"`,
		`"idempotency-key"`} {
		assert.Contains(t, req.Header.Get("Signature-Input"), c)
	}
	_, err := r.VerifyRequest(caller(t), req, raw, testWindow, time.Now())
	require.NoError(t, err)
}

// A caller that leaves while its refresh is fetched gets its context's
// error (the middleware answers it 503); the refresh goes on, is cached,
// and is the one the floor counts.
func TestARefreshOutlivesItsCaller(t *testing.T) {
	key, rotated := testSigningKey(t), testSigningKey(t)
	first := withKeys(t, platformProfileJSON(t, "https://p.example/h"),
		key.JWK())
	second := withKeys(t, platformProfileJSON(t, "https://p.example/h"),
		key.JWK(), rotated.JWK())
	var hold atomic.Bool
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	var released sync.Once
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		if !hold.Load() {
			_, _ = w.Write([]byte(first))
			return
		}
		arrived <- struct{}{}
		<-release
		_, _ = w.Write([]byte(second))
	})
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	r := newTestResolver(t)
	url := srv.URL + "/profile.json"
	_, err := r.Resolve(caller(t), url)
	require.NoError(t, err)

	hold.Store(true)
	ctx, cancel := context.WithCancel(caller(t))
	req, raw := signedRequest(t, rotated, url)
	verified := make(chan error, 1)
	go func() {
		_, err := r.VerifyRequest(ctx, req, raw, testWindow, time.Now())
		verified <- err
	}()
	<-arrived
	cancel()
	require.ErrorIs(t, <-verified, context.Canceled)

	released.Do(func() { close(release) })
	require.Eventually(t, func() bool {
		p, _, ok := r.cached(url)
		if !ok || p == nil {
			return false
		}
		_, published := p.key(rotated.KID)
		return published
	}, 5*time.Second, 10*time.Millisecond, "the refresh was cached")
	_, err = r.Refresh(caller(t), url)
	require.ErrorIs(t, err, errRefreshSpent)
}
