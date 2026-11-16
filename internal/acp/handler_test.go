package acp

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func authTenant(schema, token string) *tenant.Tenant {
	return &tenant.Tenant{
		SchemaName:     schema,
		ACPBearerToken: token,
		Domain:         schema + ".example.test",
	}
}

// The spec's request item has no quantity, so omitting one buys one. One
// that is sent is a count, and the lines of one product are one line of
// their sum (checkout.MergeLines).
func TestCartLinesQuantity(t *testing.T) {
	lines, err := cartLines([]ItemRef{
		{ID: "1"},
		{ID: "2", Quantity: new(3)},
		{ID: "1", Quantity: new(2)},
	})
	require.NoError(t, err)
	assert.Equal(t, []checkout.Line{
		{ProductID: 1, Quantity: 3},
		{ProductID: 2, Quantity: 3},
	}, lines)

	for _, q := range []int{0, -1} {
		_, err := cartLines([]ItemRef{{ID: "1", Quantity: new(2)},
			{ID: "1", Quantity: new(q)}})
		require.ErrorIs(t, err, checkout.ErrInvalidQuantity, q)
		rec := httptest.NewRecorder()
		upstreamError(rec, err)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.JSONEq(t, `{"type":"invalid_request","code":"invalid",`+
			`"param":"$.line_items","message":"Every line item's quantity `+
			`must be at least 1, and a product's lines together at most `+
			`9007199254740991."}`, rec.Body.String())
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

	log := slog.New(slog.DiscardHandler)
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

// decodeBody keeps ignoring unknown members for forward compatibility,
// but the discount extension's "discounts" object must decode.
func TestDecodeBodyReadsDiscounts(t *testing.T) {
	t.Run("create request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/acp/checkout_sessions",
			strings.NewReader(`{
				"line_items": [{"id": "1", "quantity": 2}],
				"currency": "eur",
				"discounts": {"codes": ["SAVE10", "EXTRA"]},
				"some_future_field": {"ignored": true}
			}`))
		rec := httptest.NewRecorder()
		var out CreateRequest
		_, ok := decodeBody(rec, req, &out)
		require.True(t, ok)
		require.NotNil(t, out.Discounts)
		assert.Equal(t, []string{"SAVE10", "EXTRA"}, out.Discounts.Codes)
	})

	t.Run("update request distinguishes clear from absent", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/acp/checkout_sessions/x",
			strings.NewReader(`{"discounts": {"codes": []}}`))
		rec := httptest.NewRecorder()
		var out UpdateRequest
		_, ok := decodeBody(rec, req, &out)
		require.True(t, ok)
		require.NotNil(t, out.Discounts)
		require.NotNil(t, out.Discounts.Codes, "empty array must decode as clear")
		assert.Empty(t, out.Discounts.Codes)

		req = httptest.NewRequest(http.MethodPost, "/acp/checkout_sessions/x",
			strings.NewReader(`{"buyer": {"email": "a@b.test"}}`))
		var untouched UpdateRequest
		_, ok = decodeBody(httptest.NewRecorder(), req, &untouched)
		require.True(t, ok)
		assert.Nil(t, untouched.Discounts,
			"absent discounts must not touch the coupon")
	})
}

func TestAuthWithoutTenantIs404(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	h := &Handler{log: log}
	req := httptest.NewRequest(http.MethodGet, "/acp/checkout_sessions/x", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	h.auth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// A session's cart Django no longer serves (a guest cart since deleted)
// is a missing resource, not an outage.
func TestUpstreamErrorReportsAnUnreachableCartAsNotFound(t *testing.T) {
	rec := httptest.NewRecorder()

	upstreamError(rec, fmt.Errorf("sync: %w", django.ErrCartUnreachable))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"not_found"`)
}

// A complete of a session that is not open is answered by what the
// session is, before any lookup, whatever its state: never with an empty
// answer.
func TestACompleteOfASessionThatIsNotOpenIsAnswered(t *testing.T) {
	h := testHandler(t, unreachableRedis(t))
	tn := testTenant()
	for _, status := range []checkout.Status{
		checkout.StatusCompleteInProgress, checkout.StatusCompleted,
		checkout.StatusRequiresEscalation, checkout.StatusCanceled,
	} {
		t.Run(string(status), func(t *testing.T) {
			s := checkout.NewSession("public", tn.Domain, checkout.ProtocolACP,
				"29eb4495-e018-45e7-b59c-6646302bd4ef")
			s.Status = status
			if s.MayHaveOrder() && status != checkout.StatusCompleteInProgress {
				s.OrderID, s.OrderUUID = 684, "b9be45e5-6062-4976-ae7b-2c31eb2ad689"
			}
			r := httptest.NewRequest(http.MethodPost,
				"/acp/checkout_sessions/"+s.ID+"/complete", nil)
			a := h.completeSession(r, tn, s, CompleteRequest{})
			w := httptest.NewRecorder()
			a.write(w)
			assert.NotEmpty(t, w.Body.String(), "an empty answer")
		})
	}
}
