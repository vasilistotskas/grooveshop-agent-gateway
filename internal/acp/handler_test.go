package acp

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func authTenant(schema, token string) *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			SchemaName:     schema,
			ACPBearerToken: token,
		},
		Domain: schema + ".example.test",
	}
}

func TestAuthPerTenantBearer(t *testing.T) {
	const (
		tokenA = "platform-token-tenant-a"
		tokenB = "platform-token-tenant-b"
		// formerEnvToken is the value the removed ACP_BEARER_TOKEN
		// fallback used to accept; it must authorize nothing now.
		formerEnvToken = "legacy-env-token"
	)
	cases := []struct {
		name       string
		tenant     *tenant.Tenant
		authHeader string
		wantStatus int
	}{
		{
			name:       "tenant token accepted on own host",
			tenant:     authTenant("alpha", tokenA),
			authHeader: "Bearer " + tokenA,
			wantStatus: http.StatusOK,
		},
		{
			name:       "tenant A token rejected on tenant B host",
			tenant:     authTenant("beta", tokenB),
			authHeader: "Bearer " + tokenA,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong token rejected",
			tenant:     authTenant("alpha", tokenA),
			authHeader: "Bearer nope",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing authorization header rejected",
			tenant:     authTenant("alpha", tokenA),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "former env token rejected on an enrolled tenant",
			tenant:     authTenant("alpha", tokenA),
			authHeader: "Bearer " + formerEnvToken,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "tokenless tenant (ACP disabled) rejects former env token",
			tenant:     authTenant("alpha", ""),
			authHeader: "Bearer " + formerEnvToken,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "tokenless tenant rejects another tenant's token",
			tenant:     authTenant("alpha", ""),
			authHeader: "Bearer " + tokenA,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "empty bearer never matches a tokenless tenant",
			tenant:     authTenant("alpha", ""),
			authHeader: "Bearer ",
			wantStatus: http.StatusUnauthorized,
		},
	}

	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{log: log}
			next := func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}
			req := httptest.NewRequest(http.MethodGet,
				"/acp/checkout_sessions/x", nil)
			req = req.WithContext(
				tenant.NewContext(req.Context(), tc.tenant))
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()
			h.auth(next).ServeHTTP(rec, req)
			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

func TestAuthWithoutTenantIs404(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
	h := &Handler{log: log}
	req := httptest.NewRequest(http.MethodGet, "/acp/checkout_sessions/x", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	h.auth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
