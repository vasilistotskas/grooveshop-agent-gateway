package acp

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// specError checks that rec holds a protocol Error the vendored schema
// accepts, with the status and code wanted, and returns its body.
func specError(
	t *testing.T, rec *httptest.ResponseRecorder, status int, code string,
) map[string]any {
	t.Helper()
	require.Equal(t, status, rec.Code, "%s", rec.Body)
	require.NoError(t, compileACP(t, "Error").Validate(
		roundTrip(t, json.RawMessage(rec.Body.Bytes()))), "%s", rec.Body)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, code, body["code"])
	return body
}

// unreachableRedis answers every command with a connection error at once.
func unreachableRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: time.Second,
		DialerRetryTimeout: time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func testHandler(t *testing.T, rdb *redis.Client) *Handler {
	t.Helper()
	dj := fixtureDjango(t)
	store := checkout.NewStore(rdb)
	return NewHandler(dj, store,
		checkout.NewFlow(dj, store, testSite, quietLog()), rdb, testSite,
		quietLog())
}

// Every error internal/acp answers before it reaches Redis, or when Redis
// is down, is a spec Error, driven through the mounted routes. (What the
// middleware answers before them, such as an unknown store, is not
// internal/acp's.)
func TestRoutesAnswerSpecErrors(t *testing.T) {
	const (
		token = "platform-token"
		// offHost is a store with ACP off: no bearer is its token.
		offHost = "off.example.test"
	)
	tn := testTenant()
	tn.ACPBearerToken = token
	off := testTenant()
	mux := http.NewServeMux()
	testHandler(t, unreachableRedis(t)).Register(mux,
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				store := tn
				if r.Host == offHost {
					store = off
				}
				next.ServeHTTP(w, r.WithContext(
					tenant.NewContext(r.Context(), store)))
			})
		})
	const session = "/acp/checkout_sessions/" +
		"6f1c2a52-3d8e-4c55-9a0f-0c6a2b9e1d11"
	createBody := `{"line_items":[{"id":"1"}],"currency":"eur"}`

	cases := []struct {
		name, method, path, body, host string
		headers                        map[string]string
		status                         int
		code                           string
	}{
		{
			name: "wrong bearer", method: http.MethodGet, path: session,
			headers: map[string]string{"Authorization": "Bearer nope"},
			status:  http.StatusUnauthorized, code: "unauthorized",
		},
		// The bearer is checked before the version: a caller without a
		// valid one learns nothing else.
		{
			name: "wrong bearer and no API-Version", method: http.MethodGet,
			path: session,
			headers: map[string]string{
				"Authorization": "Bearer nope", "API-Version": "",
			},
			status: http.StatusUnauthorized, code: "unauthorized",
		},
		{
			name:   "no bearer and an unsupported API-Version",
			method: http.MethodPost, path: "/acp/checkout_sessions",
			body: createBody,
			headers: map[string]string{
				"Authorization": "", "API-Version": "2025-09-29",
			},
			status: http.StatusUnauthorized, code: "unauthorized",
		},
		{
			name:   "a store with ACP off, with or without API-Version",
			method: http.MethodGet, path: session, host: offHost,
			headers: map[string]string{"API-Version": ""},
			status:  http.StatusUnauthorized, code: "unauthorized",
		},
		{
			name:   "a store with ACP off and a valid API-Version",
			method: http.MethodGet, path: session, host: offHost,
			status: http.StatusUnauthorized, code: "unauthorized",
		},
		{
			name: "no API-Version", method: http.MethodGet, path: session,
			headers: map[string]string{"API-Version": ""},
			status:  http.StatusBadRequest, code: "missing_api_version",
		},
		{
			name:   "an API-Version this server does not implement",
			method: http.MethodPost, path: "/acp/checkout_sessions",
			body:    createBody,
			headers: map[string]string{"API-Version": "2025-09-29"},
			status:  http.StatusBadRequest, code: "unsupported_api_version",
		},
		{
			name: "a body that is not JSON", method: http.MethodPost,
			path: "/acp/checkout_sessions", body: "{",
			status: http.StatusBadRequest, code: "invalid",
		},
		{
			name: "no Idempotency-Key", method: http.MethodPost,
			path: "/acp/checkout_sessions", body: createBody,
			headers: map[string]string{"Idempotency-Key": ""},
			status:  http.StatusBadRequest, code: "idempotency_key_required",
		},
		{
			name:   "an Idempotency-Key over 255 characters",
			method: http.MethodPost, path: "/acp/checkout_sessions",
			body: createBody,
			headers: map[string]string{
				"Idempotency-Key": strings.Repeat("k", 256),
			},
			status: http.StatusBadRequest, code: "invalid",
		},
		{
			name: "Redis down on a create", method: http.MethodPost,
			path: "/acp/checkout_sessions", body: createBody,
			status: http.StatusServiceUnavailable, code: "unavailable",
		},
		{
			name: "Redis down on a read", method: http.MethodGet,
			path: session, status: http.StatusServiceUnavailable,
			code: "unavailable",
		},
		{
			name: "Redis down on a cancel", method: http.MethodPost,
			path: session + "/cancel", body: "{}",
			status: http.StatusServiceUnavailable, code: "unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path,
				strings.NewReader(tc.body))
			if tc.host != "" {
				req.Host = tc.host
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("API-Version", Version)
			req.Header.Set("Idempotency-Key", "key-1")
			for name, value := range tc.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			body := specError(t, rec, tc.status, tc.code)
			if strings.HasSuffix(tc.code, "_api_version") {
				assert.Equal(t, []any{Version}, body["supported_versions"])
			} else {
				assert.NotContains(t, body, "supported_versions")
			}
		})
	}
}

// exportedSentinels lists the exported Err* variables the non-test
// sources of the package in dir declare, as "<package>.<name>".
func exportedSentinels(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	var names []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					if strings.HasPrefix(name.Name, "Err") &&
						name.IsExported() {
						names = append(names, f.Name.Name+"."+name.Name)
					}
				}
			}
		}
	}
	return names
}

// Every error a completion can return has a deliberate answer: a sentinel
// added to checkout or to the Django client fails here until it has one.
func TestCompleteErrorMapsEverySentinel(t *testing.T) {
	type answer struct {
		err    error
		status int
		code   string
	}
	want := map[string]answer{
		"checkout.ErrNotFound": {
			checkout.ErrNotFound, http.StatusNotFound, "not_found"},
		"checkout.ErrLocked": {
			checkout.ErrLocked, http.StatusConflict, "idempotency_in_flight"},
		"checkout.ErrCompletionInProgress": {
			checkout.ErrCompletionInProgress, http.StatusConflict,
			"idempotency_in_flight"},
		"checkout.ErrNotReady": {
			checkout.ErrNotReady, http.StatusBadRequest, "missing"},
		"checkout.ErrPaymentMethodUnsupported": {
			checkout.ErrPaymentMethodUnsupported, http.StatusBadRequest,
			"unsupported"},
		"checkout.ErrPayWayUnavailable": {
			checkout.ErrPayWayUnavailable, http.StatusBadRequest,
			"unsupported"},
		"checkout.ErrOrderOutcomeUnknown": {
			checkout.ErrOrderOutcomeUnknown, http.StatusBadGateway,
			"order_outcome_unknown"},
		"checkout.ErrPaymentSessionFailed": {
			checkout.ErrPaymentSessionFailed, http.StatusBadGateway,
			"payment_link_failed"},
		"checkout.ErrSessionNotSaved": {
			checkout.ErrSessionNotSaved, http.StatusInternalServerError,
			"order_placed"},
		"django.ErrNotFound": {
			django.ErrNotFound, http.StatusBadRequest, "not_found"},
		"django.ErrCartUnreachable": {
			django.ErrCartUnreachable, http.StatusBadRequest, "not_found"},
		"django.ErrCartTokenRefused": {
			django.ErrCartTokenRefused, http.StatusBadRequest, "not_found"},
		"django.ErrValidation": {
			django.ErrValidation, http.StatusBadRequest, "invalid"},
		"django.ErrInvalidCartToken": {
			django.ErrInvalidCartToken, http.StatusBadRequest, "invalid"},
		"django.ErrConflict": {
			django.ErrConflict, http.StatusBadRequest, "invalid"},
		"django.ErrForbidden": {
			django.ErrForbidden, http.StatusBadRequest, "invalid"},
		"django.ErrUnauthorized": {
			django.ErrUnauthorized, http.StatusBadRequest, "invalid"},
		"django.ErrThrottled": {
			django.ErrThrottled, http.StatusTooManyRequests, "rate_limited"},
		"django.ErrUpstreamDown": {
			django.ErrUpstreamDown, http.StatusServiceUnavailable,
			"upstream_unavailable"},
	}
	declared := append(exportedSentinels(t, filepath.Join("..", "checkout")),
		exportedSentinels(t, filepath.Join("..", "django"))...)
	require.ElementsMatch(t, slices.Collect(maps.Keys(want)), declared,
		"every sentinel needs a deliberate answer here")

	h := testHandler(t, unreachableRedis(t))
	session := checkout.NewSession("public", "shop.example.test",
		checkout.ProtocolACP, "29eb4495-e018-45e7-b59c-6646302bd4ef")
	session.Status = checkout.StatusCompleteInProgress
	session.UpdatedAt = time.Now()
	session.OrderUUID = "b9be45e5-6062-4976-ae7b-2c31eb2ad689"
	for name, a := range want {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.completeError(rec, session, fmt.Errorf("wrapped: %w", a.err))
			specError(t, rec, a.status, a.code)
			if a.status == http.StatusConflict {
				after, err := strconv.Atoi(rec.Header().Get("Retry-After"))
				require.NoError(t, err)
				assert.Positive(t, after)
			}
		})
	}

	t.Run("a stock shortfall names the lines", func(t *testing.T) {
		shortfall := &django.StockShortfall{}
		shortfall.FailedItems = append(shortfall.FailedItems, struct {
			ProductID   int64  `json:"productId"`
			ProductName string `json:"productName"`
			Available   int    `json:"available"`
			Requested   int    `json:"requested"`
		}{ProductID: 1, ProductName: "Ζυμαρικά", Available: 1, Requested: 2})
		rec := httptest.NewRecorder()
		h.completeError(rec, session, shortfall)
		body := specError(t, rec, http.StatusBadRequest, "out_of_stock")
		assert.Contains(t, body["message"], "Ζυμαρικά: requested 2")
	})

	// A completion that has had its whole budget and still reads in
	// progress never recorded its outcome: no Retry-After brings it.
	t.Run("a completion past its budget is an unknown outcome",
		func(t *testing.T) {
			stuck := *session
			stuck.UpdatedAt = time.Now().Add(-time.Hour)
			rec := httptest.NewRecorder()
			h.completeError(rec, &stuck, checkout.ErrCompletionInProgress)
			specError(t, rec, http.StatusBadGateway, "order_outcome_unknown")
			assert.Empty(t, rec.Header().Get("Retry-After"))
		})

	// A save stamped in the future (a writer whose clock runs fast) is in
	// flight, never an unknown outcome, and waits at most one budget.
	t.Run("a deadline far ahead waits one budget at most", func(t *testing.T) {
		ahead := *session
		ahead.UpdatedAt = time.Now().Add(time.Hour)
		rec := httptest.NewRecorder()
		h.completeError(rec, &ahead, checkout.ErrCompletionInProgress)
		specError(t, rec, http.StatusConflict, "idempotency_in_flight")
		after, err := strconv.Atoi(rec.Header().Get("Retry-After"))
		require.NoError(t, err)
		budget := time.Until(h.flow.CompletionDeadline(&checkout.Session{
			UpdatedAt: time.Now(),
		}))
		assert.Positive(t, after)
		assert.LessOrEqual(t, after, int(math.Ceil(budget.Seconds())))
	})

	// The order exists: the answer names it, so the platform can find it
	// without ordering again.
	t.Run("an unsaved session names its order", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.completeError(rec, session, checkout.ErrSessionNotSaved)
		body := specError(t, rec, http.StatusInternalServerError,
			"order_placed")
		assert.Contains(t, body["message"], session.OrderUUID)
	})
}

// Create keys live under a versioned scope: a record of another shape,
// written by an older pod, is under another key and never read.
func TestCreateKeyIsVersioned(t *testing.T) {
	assert.Equal(t, "ag:alpha:acp:idem:create.v2:k",
		idemRedisKey("alpha", createScope, "k"))
}

// A 405 names what its target still serves, even when that is nothing.
func TestWriteClosedNamesTheAllowedMethods(t *testing.T) {
	for _, allow := range []string{"", "GET, HEAD"} {
		rec := httptest.NewRecorder()
		writeClosed(rec, allow, "Session is canceled.")
		specError(t, rec, http.StatusMethodNotAllowed, "conflict")
		values, ok := rec.Header()["Allow"]
		require.True(t, ok, "Allow %q must be sent", allow)
		assert.Equal(t, []string{allow}, values)
	}
}

// A session no record is stored for may have placed an order: the 404
// says so, and what to check before another session is created.
func TestAMissingSessionMayHavePlacedAnOrder(t *testing.T) {
	h := testHandler(t, unreachableRedis(t))
	rec := httptest.NewRecorder()

	h.sessionError(rec, fmt.Errorf("load: %w", checkout.ErrNotFound))

	body := specError(t, rec, http.StatusNotFound, "not_found")
	assert.Contains(t, body["message"], "an order may exist")
	assert.Contains(t, body["message"], "before creating another session")
}
