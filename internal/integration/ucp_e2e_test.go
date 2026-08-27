//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/server"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

const (
	fixtureCartID    = "29eb4495-e018-45e7-b59c-6646302bd4ef"
	fixtureOrderUUID = "b9be45e5-6062-4976-ae7b-2c31eb2ad689"
	vivaCheckoutURL  = "https://demo.vivapayments.com/web/checkout?ref=e2e42"
	internalSecret   = "e2e-internal-secret"
	// acpBearerToken is the tenant's own token as recorded in
	// tenant_resolve_webside.json.
	acpBearerToken = "acp-bearer-webside-fixture"
)

// webhookSink records signed order webhooks a platform would receive.
type webhookSink struct {
	mu       sync.Mutex
	bodies   [][]byte
	sigs     []string
	keyIDs   []string
	received chan struct{}
}

func newWebhookSink() *webhookSink {
	return &webhookSink{received: make(chan struct{}, 16)}
}

func (s *webhookSink) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.sigs = append(s.sigs, r.Header.Get("UCP-Signature"))
		s.keyIDs = append(s.keyIDs, r.Header.Get("UCP-Key-Id"))
		s.mu.Unlock()
		s.received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	})
}

// fakeCheckoutDjango extends the shared fake with the order-placement
// endpoints the checkout flow drives.
func fakeCheckoutDjango(t *testing.T) http.Handler {
	t.Helper()
	outer := http.NewServeMux()
	outer.HandleFunc("POST /api/v1/cart/reserve-stock",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(
				`{"reservationIds": [77], "message": "reserved"}`))
		})
	outer.HandleFunc("POST /api/v1/order",
		func(w http.ResponseWriter, r *http.Request) {
			// The cart rides the header, never the body.
			require.Equal(t, fixtureCartID, r.Header.Get("X-Cart-Id"))
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.NotEmpty(t, body["payWayId"])
			require.NotEmpty(t, body["email"])
			serveFixture(t, w, "order_by_uuid.json")
		})
	outer.HandleFunc("POST /api/v1/order/684/create_checkout_session",
		func(w http.ResponseWriter, r *http.Request) {
			// Guest authorization rides ?uuid=.
			require.Equal(t, fixtureOrderUUID, r.URL.Query().Get("uuid"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"sessionId": "vivasession42",
				"checkoutUrl": "` + vivaCheckoutURL + `",
				"status": "PENDING",
				"amount": "929.36",
				"currency": "EUR",
				"provider": "viva_wallet"
			}`))
		})
	inner := fakeDjangoMux(t)
	outer.Handle("/", inner)
	return outer
}

// startUCPGateway boots the full stack with real Redis, the webhook
// dispatcher running, and the internal events route armed.
func startUCPGateway(t *testing.T) (*httptest.Server, *ucp.SigningKey) {
	t.Helper()
	djangoSrv := httptest.NewServer(fakeCheckoutDjango(t))
	t.Cleanup(djangoSrv.Close)
	rdb := startRedis(t)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError + 1,
	}))
	metrics := obs.NewMetrics()
	cfg := config.Config{
		// Registers httptest webhook endpoints on 127.0.0.1.
		Env:              "test",
		DjangoBaseURL:    djangoSrv.URL + "/api/v1",
		DjangoPublicHost: "api.example.test",
		InternalSecret:   internalSecret,
		AssetsHost:       "assets.platform.test",
		MediaURLTemplate: "https://{assets_host}/media_stream-image/{path}" +
			"/800/800/contain/entropy/transparent/5/80.webp",
		TenantCacheTTL:   time.Minute,
		NegativeCacheTTL: time.Minute,
		UpstreamTimeout:  5 * time.Second,
		RateLimitPerMin:  6000,
		RateLimitBurst:   1000,
	}
	dj := django.New(cfg.DjangoBaseURL, cfg.DjangoPublicHost, internalSecret,
		cfg.UpstreamTimeout, log, metrics)
	resolver := tenant.NewResolver(dj, rdb,
		cfg.TenantCacheTTL, cfg.NegativeCacheTTL, log, metrics)

	keys := ucp.NewKeys(rdb)
	// The fixture tenant's key, for signature/KID assertions.
	key, err := keys.ForSchema(context.Background(), "webside")
	require.NoError(t, err)
	dispatcher := ucp.NewDispatcher(rdb, keys, log)
	ctx, cancel := context.WithCancel(context.Background())
	go dispatcher.Run(ctx)
	t.Cleanup(cancel)

	handler := server.New(server.Deps{
		Cfg: cfg, Log: log, Metrics: metrics, Redis: rdb,
		Django: dj, Resolver: resolver, Version: "test",
		Keys: keys, Dispatcher: dispatcher,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, key
}

func TestUCPEndToEnd(t *testing.T) {
	gw, key := startUCPGateway(t)
	session := connectMCP(t, gw.URL)
	sink := newWebhookSink()
	platform := httptest.NewServer(sink.handler())
	t.Cleanup(platform.Close)

	// Canonical request members: the buyer object is snake_case on the
	// wire, and every call carries meta.ucp-agent.profile.
	buyer := map[string]any{
		"first_name": "Μαρία", "last_name": "Παπαδοπούλου",
		"email": "maria@example.test", "phone_number": "+306912345678",
	}
	meta := func(idem string) map[string]any {
		m := map[string]any{
			"ucp-agent": map[string]any{
				"profile": "https://agent.example.test/profile.json",
			},
		}
		if idem != "" {
			m["idempotency-key"] = idem
		}
		return m
	}
	codInstrument := map[string]any{
		"instruments": []any{map[string]any{
			"handler_id": ucp.HandlerID,
			"type":       ucp.InstrumentCashOnDelivery,
			"selected":   true,
		}},
	}
	fulfillment := map[string]any{
		"kind": "home_delivery", "providerCode": "acs",
		"countryCode": "GR", "city": "Αθήνα", "zipcode": "10431",
		"street": "Πανεπιστημίου", "streetNumber": "12",
	}

	t.Run("well-known profile serves the signing key", func(t *testing.T) {
		resp, err := http.Get(gw.URL + "/.well-known/ucp")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "public, max-age=300",
			resp.Header.Get("Cache-Control"))
		var profile struct {
			UCP  map[string]any      `json:"ucp"`
			Keys []map[string]string `json:"keys"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&profile))
		require.Len(t, profile.Keys, 1)
		assert.Equal(t, key.KID, profile.Keys[0]["kid"])
		assert.Equal(t, ucp.Version, profile.UCP["version"])
	})

	t.Run("cash-on-delivery checkout completes without escalation",
		func(t *testing.T) {
			res := callTool(t, session, "create_checkout", map[string]any{
				"meta":     meta(""),
				"cart_id":  fixtureCartID,
				"checkout": map[string]any{},
			})
			require.False(t, res.IsError)
			created := structured(t, res)
			assert.Equal(t, "incomplete", created["status"])
			checkoutID := created["id"].(string)

			res = callTool(t, session, "update_checkout", map[string]any{
				"meta": meta(""),
				"id":   checkoutID,
				"checkout": map[string]any{
					"buyer":       buyer,
					"fulfillment": fulfillment,
					// Selecting the advertised instrument is how an agent
					// says "cash on delivery" — no store-specific id.
					"payment": codInstrument,
				},
			})
			require.False(t, res.IsError)
			updated := structured(t, res)
			assert.Equal(t, "ready_for_complete", updated["status"])
			// Totals: 92936 subtotal; COD fee waived above its threshold.
			totals := updated["totals"].([]any)
			first := totals[0].(map[string]any)
			assert.Equal(t, "subtotal", first["type"])
			assert.EqualValues(t, 92936, first["amount"])

			res = callTool(t, session, "complete_checkout", map[string]any{
				"meta":     meta("11111111-1111-1111-1111-111111111111"),
				"id":       checkoutID,
				"checkout": map[string]any{"payment": codInstrument},
			})
			require.False(t, res.IsError)
			completed := structured(t, res)
			assert.Equal(t, "completed", completed["status"])
			order := completed["order"].(map[string]any)
			assert.Equal(t, fixtureOrderUUID, order["id"])
			assert.Contains(t, order["permalink_url"],
				"/checkout/success/"+fixtureOrderUUID)

			// Repeat completes are idempotent reads of the final state.
			res = callTool(t, session, "complete_checkout", map[string]any{
				"meta":     meta("11111111-1111-1111-1111-111111111111"),
				"id":       checkoutID,
				"checkout": map[string]any{"payment": codInstrument},
			})
			require.False(t, res.IsError)
			assert.Equal(t, "completed", structured(t, res)["status"])
		})

	t.Run("viva checkout escalates, then the order event completes it "+
		"and signs the platform webhook", func(t *testing.T) {
		res := callTool(t, session, "create_checkout", map[string]any{
			"meta":        meta(""),
			"cart_id":     fixtureCartID,
			"webhook_url": platform.URL + "/ucp/orders",
			"checkout": map[string]any{
				"buyer":       buyer,
				"fulfillment": fulfillment,
				// An ONLINE method has no advertised instrument, so it is
				// named by id until that option is modelled.
				"pay_way_id": 2, // viva_wallet, hosted authorization
			},
		})
		require.False(t, res.IsError)
		created := structured(t, res)
		require.Equal(t, "ready_for_complete", created["status"])
		checkoutID := created["id"].(string)

		res = callTool(t, session, "complete_checkout", map[string]any{
			"meta":     meta("22222222-2222-2222-2222-222222222222"),
			"id":       checkoutID,
			"checkout": map[string]any{},
		})
		require.False(t, res.IsError)
		escalated := structured(t, res)
		assert.Equal(t, "requires_escalation", escalated["status"])
		assert.Equal(t, vivaCheckoutURL, escalated["continue_url"])

		// Django's Celery task pushes the payment-completed event.
		event, err := json.Marshal(map[string]any{
			"schemaName":    "webside",
			"orderUuid":     fixtureOrderUUID,
			"status":        "PROCESSING",
			"paymentStatus": "COMPLETED",
		})
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost,
			gw.URL+"/internal/events/order-status", bytes.NewReader(event))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-Token", internalSecret)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusNoContent, resp.StatusCode)

		// The dispatcher delivers the signed webhook to the platform.
		select {
		case <-sink.received:
		case <-time.After(15 * time.Second):
			t.Fatal("platform webhook was never delivered")
		}
		sink.mu.Lock()
		body, sig, kid := sink.bodies[0], sink.sigs[0], sink.keyIDs[0]
		sink.mu.Unlock()

		assert.Equal(t, key.KID, kid)
		rawSig, err := base64.RawURLEncoding.DecodeString(sig)
		require.NoError(t, err)
		assert.True(t, ed25519.Verify(key.Public, body, rawSig),
			"webhook signature must verify against the profile JWK")
		var delivered map[string]any
		require.NoError(t, json.Unmarshal(body, &delivered))
		assert.Equal(t, fixtureOrderUUID, delivered["order_uuid"])
		assert.Equal(t, "COMPLETED", delivered["payment_status"])
		assert.Equal(t, checkoutID, delivered["checkout_id"])

		// The session is now terminal; complete re-renders the outcome.
		res = callTool(t, session, "complete_checkout", map[string]any{
			"meta":     meta("22222222-2222-2222-2222-222222222222"),
			"id":       checkoutID,
			"checkout": map[string]any{},
		})
		require.False(t, res.IsError)
		assert.Equal(t, "completed", structured(t, res)["status"])
	})

	t.Run("internal events route rejects a bad token", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost,
			gw.URL+"/internal/events/order-status",
			bytes.NewReader([]byte(`{}`)))
		require.NoError(t, err)
		req.Header.Set("X-Internal-Token", "wrong")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("unknown order events are acknowledged", func(t *testing.T) {
		event := []byte(`{"schemaName": "webside", "orderUuid": ` +
			`"00000000-0000-0000-0000-000000000000", ` +
			`"paymentStatus": "COMPLETED"}`)
		req, err := http.NewRequest(http.MethodPost,
			gw.URL+"/internal/events/order-status", bytes.NewReader(event))
		require.NoError(t, err)
		req.Header.Set("X-Internal-Token", internalSecret)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	})
}
