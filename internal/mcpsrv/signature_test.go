package mcpsrv

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpsig"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// signingPlatform is a platform with a signing key, whose profile
// publishes it.
type signingPlatform struct {
	key     *ucp.SigningKey
	profile string
}

func newSigningPlatform(t *testing.T) signingPlatform {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	scalar, err := priv.Bytes()
	require.NoError(t, err)
	key, err := ucp.SigningKeyFromScalar(scalar)
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata",
		"fixtures", "ucp", "platform_profile.json"))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.ReplaceAll(string(raw),
		"{{webhook_url}}", "http://127.0.0.1:1/ucp/orders")), &doc))
	doc["keys"] = []map[string]string{key.JWK()}
	body, err := json.Marshal(doc)
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(body)
		}))
	t.Cleanup(srv.Close)
	return signingPlatform{key: key, profile: srv.URL + "/profile.json"}
}

// post is a JSON-RPC body to the store's /mcp, signed by p unless p is
// nil.
func (p *signingPlatform) post(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(body))
	req.Host = "shop.example"
	req.Header.Set("Content-Type", "application/json")
	if p == nil {
		return req
	}
	require.NoError(t, ucp.SignRequest(req, []byte(body), p.profile, p.key,
		time.Now()))
	return req
}

// callBody is a tools/call of tool with arguments args, id 7.
func callBody(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	require.NoError(t, err)
	return string(b)
}

// signatureGate is VerifyPlatformSignature in front of a handler that
// records what reached it.
type signatureGate struct {
	handler http.Handler
	reached bool
	signer  string
	body    string
}

func newSignatureGate(t *testing.T) *signatureGate {
	t.Helper()
	profiles, err := ucp.NewProfileResolver(true,
		slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	g := &signatureGate{}
	g.handler = VerifyPlatformSignature(Deps{
		Profiles:        profiles,
		SignatureWindow: httpsig.Window{MaxAge: time.Minute, Skew: time.Minute},
		Log:             slog.New(slog.DiscardHandler),
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.reached = true
		g.signer, _ = signerFrom(r.Context())
		b, _ := io.ReadAll(r.Body)
		g.body = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	return g
}

// sigRefusal is a JSON-RPC error answer as the middleware writes it.
type sigRefusal struct {
	ID    any `json:"id"`
	Error struct {
		Code    int64  `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Code    string `json:"code"`
			Content string `json:"content"`
		} `json:"data"`
	} `json:"error"`
}

func (g *signatureGate) serve(
	t *testing.T, req *http.Request,
) (*httptest.ResponseRecorder, sigRefusal) {
	t.Helper()
	rec := httptest.NewRecorder()
	g.handler.ServeHTTP(rec, req)
	var out sigRefusal
	if rec.Code != http.StatusNoContent {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out),
			rec.Body.String())
	}
	return rec, out
}

func TestUnsignedGetOrderIsRefusedBeforeTheHandler(t *testing.T) {
	g := newSignatureGate(t)
	rec, out := g.serve(t, (*signingPlatform)(nil).post(t,
		callBody(t, "get_order", map[string]any{"id": "o1"})))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, g.reached)
	assert.EqualValues(t, 7, out.ID)
	assert.EqualValues(t, -32000, out.Error.Code)
	assert.Equal(t, ucpSignatureFailed, out.Error.Message)
	assert.Equal(t, httpsig.CodeSignatureMissing, out.Error.Data.Code)

	// In a batch too, answered with the null id.
	batch := "[" + callBody(t, "get_checkout", nil) + "," +
		callBody(t, "get_order", nil) + "]"
	rec, out = g.serve(t, (*signingPlatform)(nil).post(t, batch))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Nil(t, out.ID)
	assert.False(t, g.reached)
}

func TestUnsignedCallsToOtherToolsPassUntouched(t *testing.T) {
	g := newSignatureGate(t)
	body := callBody(t, "create_checkout", map[string]any{
		"meta": map[string]any{"ucp-agent": map[string]any{
			"profile": "https://platform.example/ucp"}},
	})
	rec, _ := g.serve(t, (*signingPlatform)(nil).post(t, body))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, g.reached)
	assert.Empty(t, g.signer)
	assert.Equal(t, body, g.body, "the handler reads the body as sent")

	g = newSignatureGate(t)
	rec, _ = g.serve(t, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestASignedRequestNamesItsPlatform(t *testing.T) {
	p := newSigningPlatform(t)
	g := newSignatureGate(t)
	body := callBody(t, "get_order", map[string]any{
		"meta": map[string]any{"ucp-agent": map[string]any{
			"profile": p.profile}},
		"id": "o1",
	})
	rec, _ := g.serve(t, p.post(t, body))
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, g.reached)
	want, err := ucp.NormalizeProfileURL(p.profile)
	require.NoError(t, err)
	assert.Equal(t, want, g.signer)
	assert.Equal(t, body, g.body)
}

func TestASignatureThatDoesNotVerifyIsRefusedWhateverItCalls(t *testing.T) {
	p := newSigningPlatform(t)
	other := newSigningPlatform(t)
	for name, tc := range map[string]struct {
		req    func(t *testing.T) *http.Request
		status int
		rpc    int64
		code   string
	}{
		"a body changed after signing": {
			req: func(t *testing.T) *http.Request {
				req := p.post(t, `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
				req.Body = io.NopCloser(strings.NewReader(
					`{"jsonrpc":"2.0","id":7,"method":"tools/lisT"}`))
				return req
			},
			status: http.StatusBadRequest, rpc: -32600,
			code: httpsig.CodeDigestMismatch,
		},
		"a key the named profile does not publish": {
			req: func(t *testing.T) *http.Request {
				req := other.post(t, `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
				req.Header.Set("UCP-Agent", `profile="`+p.profile+`"`)
				return req
			},
			status: http.StatusUnauthorized, rpc: -32000,
			code: httpsig.CodeKeyNotFound,
		},
		"meta naming another platform": {
			req: func(t *testing.T) *http.Request {
				return p.post(t, callBody(t, "get_checkout", map[string]any{
					"meta": map[string]any{"ucp-agent": map[string]any{
						"profile": other.profile}},
					"id": "c1",
				}))
			},
			status: http.StatusUnauthorized, rpc: -32000,
			code: httpsig.CodeSignatureInvalid,
		},
		"a signed get_order with no UCP-Agent": {
			req: func(t *testing.T) *http.Request {
				req := p.post(t, callBody(t, "get_order",
					map[string]any{"id": "o1"}))
				req.Header.Del("UCP-Agent")
				return req
			},
			status: http.StatusUnauthorized, rpc: -32000,
			code: httpsig.CodeSignatureInvalid,
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newSignatureGate(t)
			rec, out := g.serve(t, tc.req(t))
			assert.Equal(t, tc.status, rec.Code)
			assert.False(t, g.reached)
			assert.EqualValues(t, 7, out.ID)
			assert.Equal(t, tc.rpc, out.Error.Code)
			assert.Equal(t, tc.code, out.Error.Data.Code)
		})
	}
}

// A profile that cannot be fetched is the discovery failure, with the
// spec's HTTP status for it.
func TestASignerWhoseProfileCannotBeHadIsADiscoveryFailure(t *testing.T) {
	p := newSigningPlatform(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(gone.Close)
	p.profile = gone.URL + "/profile.json"

	g := newSignatureGate(t)
	rec, out := g.serve(t, p.post(t,
		`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`))
	assert.Equal(t, http.StatusFailedDependency, rec.Code)
	assert.EqualValues(t, ucpDiscoveryFailed, out.Error.Code)
	assert.Equal(t, ucp.CodeProfileUnreachable, out.Error.Data.Code)
	assert.False(t, g.reached)
}

// A signature on a request that is not UCP's (no UCP-Agent, no tool that
// serves only a verified platform) is not verified: the request is
// served as an unsigned one, whatever the signature is.
func TestASignatureOutsideUCPIsNotVerified(t *testing.T) {
	p := newSigningPlatform(t)
	g := newSignatureGate(t)
	req := p.post(t, callBody(t, "search_products",
		map[string]any{"query": "x"}))
	req.Header.Del("UCP-Agent")
	req.Header.Set("Signature", "sig1=:AAAA:")

	rec, _ := g.serve(t, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, g.reached)
	assert.Empty(t, g.signer)
}

// A caller that left while its signature was checked is answered 503,
// and nothing is logged.
func TestACallerWhoLeftIsAnswered503(t *testing.T) {
	p := newSigningPlatform(t)
	// The profile never answers, so the caller is gone before it does.
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(stalled.Close)
	t.Cleanup(func() { close(release) })
	p.profile = stalled.URL + "/profile.json"
	g := newSignatureGate(t)
	req := p.post(t, `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()

	rec, _ := g.serve(t, req.WithContext(ctx))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.False(t, g.reached)
}
