//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redis/go-redis/v9"
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
	// tenant_resolve_demostore.json.
	acpBearerToken = "acp-bearer-demostore-fixture"
)

// receivedWebhook is one order webhook as a platform receives it.
type receivedWebhook struct {
	method, host, path string
	header             http.Header
	body               []byte
}

// webhookSink records signed order webhooks a platform would receive.
type webhookSink struct {
	mu       sync.Mutex
	hooks    []receivedWebhook
	received chan struct{}
}

func newWebhookSink() *webhookSink {
	return &webhookSink{received: make(chan struct{}, 16)}
}

func (s *webhookSink) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.hooks = append(s.hooks, receivedWebhook{
			method: r.Method, host: r.Host, path: r.URL.Path,
			header: r.Header.Clone(), body: body,
		})
		s.mu.Unlock()
		s.received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	})
}

var signatureInputRe = regexp.MustCompile(`^sig1=\(([^)]*)\)(.*)$`)

// verifyWebhook checks a delivery the way a UCP platform does, from the
// wire alone: Content-Digest over the raw body, then the RFC 9421
// signature base rebuilt from the components Signature-Input lists,
// verified as ES256 against the business profile's JWK.
func verifyWebhook(t *testing.T, hook receivedWebhook, jwk map[string]string) {
	t.Helper()
	sum := sha256.Sum256(hook.body)
	assert.Equal(t,
		"sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":",
		hook.header.Get("Content-Digest"))

	input := hook.header.Get("Signature-Input")
	m := signatureInputRe.FindStringSubmatch(input)
	require.NotNil(t, m, "Signature-Input: %s", input)
	assert.Contains(t, m[2], `;keyid="`+jwk["kid"]+`"`)
	var base strings.Builder
	for _, quoted := range strings.Fields(m[1]) {
		name := strings.Trim(quoted, `"`)
		var value string
		switch name {
		case "@method":
			value = hook.method
		case "@authority":
			value = hook.host
		case "@path":
			value = hook.path
		default:
			value = hook.header.Get(name)
			require.NotEmpty(t, value, "covered header %s", name)
		}
		base.WriteString(quoted + ": " + value + "\n")
	}
	base.WriteString(`"@signature-params": (` + m[1] + ")" + m[2])

	sigValue := strings.TrimSuffix(strings.TrimPrefix(
		hook.header.Get("Signature"), "sig1=:"), ":")
	sig, err := base64.StdEncoding.DecodeString(sigValue)
	require.NoError(t, err)
	require.Len(t, sig, 64, "ES256 is raw r||s, not ASN.1")

	x, err := base64.RawURLEncoding.DecodeString(jwk["x"])
	require.NoError(t, err)
	y, err := base64.RawURLEncoding.DecodeString(jwk["y"])
	require.NoError(t, err)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(),
		append(append([]byte{0x04}, x...), y...))
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(base.String()))
	assert.True(t, ecdsa.Verify(pub, digest[:],
		new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])),
		"webhook signature must verify against the profile JWK")
}

// fakeCheckoutDjango extends the shared fake with the order-placement
// endpoints the checkout flow drives. failPaymentLink makes Viva's
// payment-session creation fail, for the order-exists-without-link path.
func fakeCheckoutDjango(t *testing.T, failPaymentLink *atomic.Bool) http.Handler {
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
			assert.Equal(t, fixtureCartID, r.Header.Get("X-Cart-Id"))
			var body map[string]any
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.NotEmpty(t, body["payWayId"])
			assert.NotEmpty(t, body["email"])
			serveFixture(t, w, "order_by_uuid.json")
		})
	outer.HandleFunc("POST /api/v1/order/684/create_checkout_session",
		func(w http.ResponseWriter, r *http.Request) {
			// Guest authorization rides ?uuid=.
			assert.Equal(t, fixtureOrderUUID, r.URL.Query().Get("uuid"))
			if failPaymentLink.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
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

// ucpStack is the full gateway over a fake Django and real Redis.
type ucpStack struct {
	gw  *httptest.Server
	key *ucp.SigningKey
	rdb *redis.Client
	// failPaymentLink toggles Viva payment-session failures upstream.
	failPaymentLink *atomic.Bool
}

// startUCPGateway boots the full stack with real Redis, the webhook
// dispatcher running, and the internal events route armed.
func startUCPGateway(t *testing.T) ucpStack {
	t.Helper()
	failPaymentLink := new(atomic.Bool)
	djangoSrv := httptest.NewServer(
		fakeCheckoutDjango(t, failPaymentLink))
	t.Cleanup(djangoSrv.Close)
	rdb := startRedis(t)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError + 1,
	}))
	metrics := obs.NewMetrics()
	cfg := config.Config{
		Env: config.EnvTest,
		// Registers httptest webhook endpoints on 127.0.0.1.
		AllowLocalWebhooks: true,
		PaymentHandlerEnv:  config.HandlerEnvSandbox,
		DjangoBaseURL:      djangoSrv.URL + "/api/v1",
		DjangoPublicHost:   "api.example.test",
		InternalSecret:     internalSecret,
		AssetsHost:         "assets.platform.test",
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
	key, err := keys.ForSchema(context.Background(), "demostore")
	require.NoError(t, err)
	dispatcher := ucp.NewDispatcher(rdb, keys, log, "e2e", true)
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
	return ucpStack{
		gw: srv, key: key, rdb: rdb, failPaymentLink: failPaymentLink,
	}
}

func TestUCPEndToEnd(t *testing.T) {
	stack := startUCPGateway(t)
	gw, key := stack.gw, stack.key
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
			"schemaName":    "demostore",
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
		hook := sink.hooks[0]
		sink.mu.Unlock()

		verifyWebhook(t, hook, key.JWK())
		// Standard Webhooks headers, and the business named as signer.
		assert.NotEmpty(t, hook.header.Get("Webhook-Id"))
		assert.NotEmpty(t, hook.header.Get("Webhook-Timestamp"))
		assert.Contains(t, hook.header.Get("UCP-Agent"),
			`profile="https://`)
		assert.Contains(t, hook.header.Get("UCP-Agent"), "/.well-known/ucp")

		// The body is the full order entity, never the queue record.
		var delivered map[string]any
		require.NoError(t, json.Unmarshal(hook.body, &delivered))
		assert.Equal(t, fixtureOrderUUID, delivered["id"])
		assert.Equal(t, checkoutID, delivered["checkout_id"])
		assert.NotEmpty(t, delivered["line_items"])
		for _, internal := range []string{"schema", "target_url", "targetUrl"} {
			assert.NotContains(t, delivered, internal)
		}

		// The session is now terminal; complete re-renders the outcome.
		res = callTool(t, session, "complete_checkout", map[string]any{
			"meta":     meta("22222222-2222-2222-2222-222222222222"),
			"id":       checkoutID,
			"checkout": map[string]any{},
		})
		require.False(t, res.IsError)
		assert.Equal(t, "completed", structured(t, res)["status"])

		// The session expires within a day; the store reports shipment
		// for weeks after. Events route through the order link, so the
		// platform still hears about them.
		require.NoError(t, stack.rdb.Del(context.Background(),
			"ag:demostore:cs:"+checkoutID).Err())
		shipped, err := json.Marshal(map[string]any{
			"schemaName": "demostore", "orderUuid": fixtureOrderUUID,
			"status": "SHIPPED", "paymentStatus": "COMPLETED",
		})
		require.NoError(t, err)
		req, err = http.NewRequest(http.MethodPost,
			gw.URL+"/internal/events/order-status", bytes.NewReader(shipped))
		require.NoError(t, err)
		req.Header.Set("X-Internal-Token", internalSecret)
		resp, err = http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		select {
		case <-sink.received:
		case <-time.After(15 * time.Second):
			t.Fatal("an event after session expiry was never delivered")
		}
		sink.mu.Lock()
		late := sink.hooks[len(sink.hooks)-1]
		sink.mu.Unlock()
		verifyWebhook(t, late, key.JWK())
		assert.NotEqual(t, hook.header.Get("Webhook-Id"),
			late.header.Get("Webhook-Id"), "each event is its own delivery")
	})

	t.Run("an online method named only at complete time places the order",
		func(t *testing.T) {
			res := callTool(t, session, "create_checkout", map[string]any{
				"meta":    meta(""),
				"cart_id": fixtureCartID,
				"checkout": map[string]any{
					"buyer": buyer, "fulfillment": fulfillment,
				},
			})
			require.False(t, res.IsError)
			created := structured(t, res)
			require.Equal(t, "incomplete", created["status"])

			res = callTool(t, session, "complete_checkout", map[string]any{
				"meta":     meta(uuid.NewString()),
				"id":       created["id"],
				"checkout": map[string]any{"pay_way_id": 2},
			})
			require.False(t, res.IsError, "complete must recompute readiness")
			escalated := structured(t, res)
			assert.Equal(t, "requires_escalation", escalated["status"])
			assert.Equal(t, vivaCheckoutURL, escalated["continue_url"])
		})

	t.Run("a failed payment link keeps the order and retries the link",
		func(t *testing.T) {
			stack.failPaymentLink.Store(true)
			res := callTool(t, session, "create_checkout", map[string]any{
				"meta":    meta(""),
				"cart_id": fixtureCartID,
				"checkout": map[string]any{
					"buyer": buyer, "fulfillment": fulfillment,
					"pay_way_id": 2,
				},
			})
			require.False(t, res.IsError)
			checkoutID := structured(t, res)["id"].(string)

			res = callTool(t, session, "complete_checkout", map[string]any{
				"meta":     meta(uuid.NewString()),
				"id":       checkoutID,
				"checkout": map[string]any{},
			})
			require.True(t, res.IsError)
			text := res.Content[0].(*mcp.TextContent).Text
			assert.Contains(t, text, "order "+fixtureOrderUUID+" is placed")
			assert.NotContains(t, text, "the cart is unchanged")

			res = callTool(t, session, "get_checkout", map[string]any{
				"meta": meta(""), "id": checkoutID,
			})
			require.False(t, res.IsError)
			pending := structured(t, res)
			assert.Equal(t, "requires_escalation", pending["status"])
			assert.Contains(t, pending["continue_url"],
				"/checkout/success/"+fixtureOrderUUID,
				"never the cart: claiming it would order twice")

			stack.failPaymentLink.Store(false)
			res = callTool(t, session, "complete_checkout", map[string]any{
				"meta":     meta(uuid.NewString()),
				"id":       checkoutID,
				"checkout": map[string]any{},
			})
			require.False(t, res.IsError)
			assert.Equal(t, vivaCheckoutURL,
				structured(t, res)["continue_url"])

			// The order exists: canceling the checkout would not cancel
			// it, and the buyer could still pay a "canceled" checkout.
			res = callTool(t, session, "cancel_checkout", map[string]any{
				"meta": meta(uuid.NewString()), "id": checkoutID,
			})
			require.True(t, res.IsError)
			assert.Contains(t, res.Content[0].(*mcp.TextContent).Text,
				"cannot be canceled")
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
		event := []byte(`{"schemaName": "demostore", "orderUuid": ` +
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
