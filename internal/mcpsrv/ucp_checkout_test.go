package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcptool"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

const fixtureOrderUUID = "b9be45e5-6062-4976-ae7b-2c31eb2ad689"

// testOrderRetry sends an unanswered order up to three times, quickly.
var testOrderRetry = checkout.OrderRetry{
	Attempts: 3, Delay: 10 * time.Millisecond,
}

// ucpMeta names a platform profile served from the recorded fixture.
func ucpMeta(t *testing.T, idem string) *MetaIn {
	t.Helper()
	return &MetaIn{UCPAgent: &UCPAgentIn{Profile: platformProfile(t)},
		IdempotencyKey: idem}
}

// platformProfiles is each test's platform profile URL (platformProfile).
var platformProfiles sync.Map

// platformProfile is the URL of the test's one platform profile, served
// for the length of the test: every call a test makes names the same
// platform, as a platform's calls do, so its profile is fetched once.
func platformProfile(t *testing.T) string {
	t.Helper()
	if url, ok := platformProfiles.Load(t); ok {
		return url.(string)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata",
		"fixtures", "ucp", "platform_profile.json"))
	require.NoError(t, err)
	body := strings.ReplaceAll(string(raw), "{{webhook_url}}",
		"http://127.0.0.1:1/ucp/orders")
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
	url := srv.URL + "/profile.json"
	platformProfiles.Store(t, url)
	t.Cleanup(func() {
		platformProfiles.Delete(t)
		srv.Close()
	})
	return url
}

// platformCtx is ctx for a call to the UCP tools at the cart tenant, as
// the rate limiter's middleware leaves it, naming the test's caller
// (httpmw.ClientFrom).
func platformCtx(t *testing.T) context.Context {
	t.Helper()
	return httpmw.WithClient(
		tenant.NewContext(t.Context(), cartTenant()), "192.0.2.10")
}

// signedAs is ctx as VerifyPlatformSignature leaves it for a request
// signed by the platform meta names.
func signedAs(
	t *testing.T, ctx context.Context, meta *MetaIn,
) context.Context {
	t.Helper()
	signer, err := ucp.NormalizeProfileURL(meta.UCPAgent.Profile)
	require.NoError(t, err)
	return context.WithValue(ctx, signerKey{}, signer)
}

// ucpDjango serves the pay ways and the placed order from their fixtures
// and records every call it gets.
func ucpDjango(t *testing.T) (*django.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls = append(calls, r.Method+" "+r.URL.Path)
			mu.Unlock()
			fixture := map[string]string{
				"/api/v1/pay_way":                        "pay_way.json",
				"/api/v1/order/uuid/" + fixtureOrderUUID: "order_by_uuid.json",
			}[r.URL.Path]
			if r.Method != http.MethodGet || fixture == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			body, err := os.ReadFile(filepath.Join("..", "..", "testdata",
				"fixtures", "django", fixture))
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}))
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
			5*time.Second, slog.New(slog.DiscardHandler), nil),
		func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), calls...)
		}
}

// ucpHandlers serve the UCP tools over dj with a checkout store that
// cannot be reached.
func ucpHandlers(t *testing.T, dj *django.Client) *handlers {
	t.Helper()
	profiles, err := ucp.NewProfileResolver(true, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 20 * time.Millisecond,
		MaxRetries: -1,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	return &handlers{deps: Deps{Site: testSite, Django: dj,
		Profiles: profiles, Checkout: checkout.NewStore(rdb, dj.Timeout()),
		AllowLocalWebhooks: true, Log: slog.New(slog.DiscardHandler)}}
}

// A payment the store refuses is refused before the cart is written:
// create_checkout neither creates a cart nor fills one. (update_checkout
// orders its steps the same way: see
// TestUpdateCheckoutSettlesEverythingBeforeTheFirstCartWrite.)
func TestCreateCheckoutRefusesThePaymentBeforeTheCart(t *testing.T) {
	for name, tc := range map[string]struct {
		instrument UCPInstrumentIn
		want       string
	}{
		"another handler's instrument": {UCPInstrumentIn{
			HandlerID: "com.example.elsewhere",
			Type:      ucp.InstrumentCashOnDelivery,
		}, "unknown payment handler_id"},
		"an instrument the store does not offer": {UCPInstrumentIn{
			HandlerID: ucp.HandlerID, Type: "card",
		}, "not available"},
	} {
		t.Run(name, func(t *testing.T) {
			dj, calls := ucpDjango(t)
			h := ucpHandlers(t, dj)
			ctx := platformCtx(t)

			_, _, err := h.createCheckout(ctx, nil, CreateCheckoutIn{
				Meta: ucpMeta(t, ""),
				Checkout: UCPCheckoutIn{
					LineItems: []UCPLineItemReqIn{{
						Item: UCPItemIn{ID: "2"}, Quantity: 1,
					}},
					Discounts: &UCPDiscountsIn{Codes: []string{"SAVE10"}},
					Payment: &UCPPaymentIn{
						Instruments: []UCPInstrumentIn{tc.instrument},
					},
				},
			})

			require.ErrorContains(t, err, tc.want)
			for _, call := range calls() {
				assert.NotContains(t, call, "/api/v1/cart")
			}
		})
	}
}

// A cart_id that is not a UUID names no cart: it is refused before a
// Redis key is built from it and before the store is asked anything. The
// checkout store cannot be reached here, so a create that went on to
// read the cart's holder would answer "unavailable".
func TestCreateCheckoutRefusesACartIDThatIsNoUUID(t *testing.T) {
	for _, id := range []string{
		"cart", "*", fixtureCartID + ":lock", fixtureCartID[:35],
	} {
		t.Run(id, func(t *testing.T) {
			dj, calls := ucpDjango(t)
			h := ucpHandlers(t, dj)
			ctx := platformCtx(t)

			_, _, err := h.createCheckout(ctx, nil, CreateCheckoutIn{
				Meta: ucpMeta(t, ""), CartID: id,
			})

			require.ErrorContains(t, err, "cart_id is not a cart id")
			assert.Empty(t, calls())
		})
	}
}

// A checkout store that cannot be reached is the store being unavailable,
// the same ToolError at every tool that reads or writes it.
func TestCheckoutStoreOutageIsUnavailable(t *testing.T) {
	id := uuid.NewString()
	for name, call := range map[string]func(
		context.Context, *handlers, *testing.T,
	) error{
		"create_checkout saves the session": func(
			ctx context.Context, h *handlers, t *testing.T,
		) error {
			_, _, err := h.createCheckout(ctx, nil, CreateCheckoutIn{
				Meta: ucpMeta(t, ""), CartID: fixtureCartID,
			})
			return err
		},
		"get_checkout reads it": func(
			ctx context.Context, h *handlers, t *testing.T,
		) error {
			_, _, err := h.getCheckout(ctx, nil,
				GetCheckoutIn{Meta: ucpMeta(t, ""), ID: id})
			return err
		},
		"update_checkout locks it": func(
			ctx context.Context, h *handlers, t *testing.T,
		) error {
			_, _, err := h.updateCheckout(ctx, nil,
				UpdateCheckoutIn{Meta: ucpMeta(t, ""), ID: id})
			return err
		},
		"complete_checkout locks it": func(
			ctx context.Context, h *handlers, t *testing.T,
		) error {
			_, _, err := h.completeCheckout(ctx, nil, CompleteCheckoutIn{
				Meta: ucpMeta(t, uuid.NewString()), ID: id,
			})
			return err
		},
		"cancel_checkout locks it": func(
			ctx context.Context, h *handlers, t *testing.T,
		) error {
			_, _, err := h.cancelCheckout(ctx, nil, CancelCheckoutIn{
				Meta: ucpMeta(t, uuid.NewString()), ID: id,
			})
			return err
		},
		"get_order reads the order link": func(
			ctx context.Context, h *handlers, t *testing.T,
		) error {
			meta := ucpMeta(t, "")
			_, _, err := h.getOrder(signedAs(t, ctx, meta), nil,
				GetOrderIn{Meta: meta, ID: fixtureOrderUUID})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			dj, _ := ucpDjango(t)
			ctx := platformCtx(t)

			err := call(ctx, ucpHandlers(t, dj), t)

			require.ErrorIs(t, err, errCheckoutUnavailable)
			var got *mcptool.ToolError
			require.ErrorAs(t, err, &got)
			assert.Equal(t, "unavailable", got.Code)
			assert.Equal(t, mcptool.UnavailableRetryAfter, got.RetryAfter)
		})
	}
}

// A checkout no session is stored for may have placed an order: its
// session can be gone by expiry, by a flush or a restore, or by a release that
// changes the session key (an ordinary rollout drops none), and the
// id does not say. An agent told only to start a new one would order a
// second time, so it is told to check first, with what it holds.
func TestAMissingCheckoutIsNotOnlyStartANewOne(t *testing.T) {
	dj, _ := ucpDjango(t)
	h := ucpHandlers(t, dj)
	ctx := platformCtx(t)

	// An id no session can have is known to be missing without the store
	// being asked, by the tools that lock the session too: these handlers'
	// store cannot be reached, so any of them that asked it would answer
	// unavailable. What a well-formed id no session is stored for gets is
	// the integration suite's (TestAMissingCheckoutMayHavePlacedAnOrder).
	for _, id := range []string{
		"not-a-session-id",
		// Another spelling of a UUID: sessions are stored under one.
		"6F1C2A52-3D8E-4C55-9A0F-0C6A2B9E1D11",
	} {
		type answer = func() (CheckoutResult, error)
		for name, call := range map[string]answer{
			"get_checkout": func() (CheckoutResult, error) {
				_, out, err := h.getCheckout(ctx, nil,
					GetCheckoutIn{Meta: ucpMeta(t, ""), ID: id})
				return out, err
			},
			"update_checkout": func() (CheckoutResult, error) {
				_, out, err := h.updateCheckout(ctx, nil,
					UpdateCheckoutIn{Meta: ucpMeta(t, ""), ID: id})
				return out, err
			},
			"complete_checkout": func() (CheckoutResult, error) {
				_, out, err := h.completeCheckout(ctx, nil,
					CompleteCheckoutIn{
						Meta: ucpMeta(t, uuid.NewString()), ID: id,
					})
				return out, err
			},
			"cancel_checkout": func() (CheckoutResult, error) {
				_, out, err := h.cancelCheckout(ctx, nil, CancelCheckoutIn{
					Meta: ucpMeta(t, uuid.NewString()), ID: id,
				})
				return out, err
			},
		} {
			t.Run(name+" "+id, func(t *testing.T) {
				out, err := call()

				// The spec's error response, not a tool error.
				require.NoError(t, err)
				require.NotNil(t, out.failure)
				assert.Nil(t, out.checkout)
				assert.Equal(t, "error", out.failure.UCP.Status)
				require.Len(t, out.failure.Messages, 1)
				m := out.failure.Messages[0]
				assert.Equal(t, "error", m.Type)
				assert.Equal(t, "not_found", m.Code)
				assert.Equal(t, "unrecoverable", m.Severity)
				for _, part := range []string{
					"an order may exist",
					"do not start another checkout before checking",
					"get_order or track_order",
					"ask the shopper for the store's order confirmation",
					"Only if it was never completed, start a new one",
				} {
					assert.Contains(t, m.Text, part)
				}
			})
		}
	}
}

// complete_checkout and cancel_checkout take an idempotency key of 1 to
// 255 visible ASCII characters, which ends a Redis key. One outside that
// form is refused, saying what form is expected, before the store is
// asked: these handlers' store cannot be reached, so a call that asked
// it would answer unavailable, as one with a key inside the bound does.
func TestCompleteAndCancelBoundTheIdempotencyKey(t *testing.T) {
	id := uuid.NewString()
	calls := map[string]func(
		context.Context, *handlers, *MetaIn,
	) error{
		"complete_checkout": func(
			ctx context.Context, h *handlers, meta *MetaIn,
		) error {
			_, _, err := h.completeCheckout(ctx, nil,
				CompleteCheckoutIn{Meta: meta, ID: id})
			return err
		},
		"cancel_checkout": func(
			ctx context.Context, h *handlers, meta *MetaIn,
		) error {
			_, _, err := h.cancelCheckout(ctx, nil,
				CancelCheckoutIn{Meta: meta, ID: id})
			return err
		},
	}
	outside := []string{
		strings.Repeat("k", checkout.MaxIdempotencyKeyLen+1),
		"a key with spaces", "tab\there", "line\nbreak", "κλειδί",
	}
	inside := []string{
		uuid.NewString(), "Zk3pQ9wL2mX7vB4nR8tY1c",
		strings.Repeat("~", checkout.MaxIdempotencyKeyLen),
	}
	for tool, call := range calls {
		t.Run(tool, func(t *testing.T) {
			dj, _ := ucpDjango(t)
			h := ucpHandlers(t, dj)
			ctx := platformCtx(t)
			for _, key := range outside {
				err := call(ctx, h, ucpMeta(t, key))
				require.ErrorIs(t, err, errIdempotencyKeyForm, key)
				assert.Contains(t, err.Error(),
					"1 to 255 visible ASCII characters", key)
			}
			for _, key := range inside {
				err := call(ctx, h, ucpMeta(t, key))
				assert.ErrorIs(t, err, errCheckoutUnavailable, key)
			}
		})
	}
}

// A completion that may still be running is a conflict an agent can
// branch on, wherever it is found: by the claim of a second complete, by
// the flow, and by any call that finds the checkout being placed. Its
// render says the same as plain text.
func TestACompletionInFlightIsAConflict(t *testing.T) {
	dj, _ := ucpDjango(t)
	h := ucpHandlers(t, dj)
	h.deps.Flow = checkout.NewFlow(dj, h.deps.Checkout, testSite,
		testOrderRetry, slog.New(slog.DiscardHandler))
	s := checkout.NewSession("demostore", "shop.test",
		checkout.ProtocolUCP, fixtureCartID)
	s.Status = checkout.StatusCompleteInProgress
	s.UpdatedAt = time.Now()

	for what, err := range map[string]error{
		"the flow":  completeErr(s, checkout.ErrCompletionInProgress),
		"an update": h.closedTo(t.Context(), s, "updated"),
		"a cancel":  h.closedTo(t.Context(), s, "canceled"),
	} {
		require.ErrorIs(t, err, errCompletionInFlight, what)
		te, ok := errors.AsType[*mcptool.ToolError](err)
		require.True(t, ok, what)
		assert.Equal(t, mcptool.CodeConflict, te.Code, what)
		assert.Contains(t, te.Hint, "in progress or unconfirmed", what)
		assert.Contains(t, te.Hint, "do not complete it again", what)
	}
	assert.Equal(t, errCompletionInFlight.Hint, guidance(
		h.pending(t.Context(), s)), "a render's text is the hint")
}

// A checkout that is not open takes no update and no cancel, whatever
// state it is in: closedTo decides on Session.Open alone, the predicate
// ACP's refuseChange decides on too.
func TestClosedToRefusesWhatIsNotOpen(t *testing.T) {
	dj, _ := ucpDjango(t)
	h := ucpHandlers(t, dj)
	h.deps.Flow = checkout.NewFlow(dj, h.deps.Checkout, testSite,
		testOrderRetry, slog.New(slog.DiscardHandler))
	for _, status := range []checkout.Status{
		checkout.StatusIncomplete, checkout.StatusReadyForComplete,
		checkout.StatusCompleteInProgress, checkout.StatusRequiresEscalation,
		checkout.StatusCompleted, checkout.StatusCanceled,
	} {
		s := checkout.NewSession("demostore", "shop.test",
			checkout.ProtocolUCP, fixtureCartID)
		s.Status = status
		for _, action := range []string{"updated", "canceled"} {
			err := h.closedTo(t.Context(), s, action)
			assert.Equal(t, !s.Open(), err != nil, "%s %s", status, action)
		}
	}
}

// Every refusal of the checkout tools carries a code an agent can branch
// on, named as ACP names the same state. An order that exists or may is
// never answered as a failure to start again: its hint says not to.
func TestCheckoutRefusalsCarryACode(t *testing.T) {
	dj, _ := ucpDjango(t)
	h := ucpHandlers(t, dj)
	h.deps.Flow = checkout.NewFlow(dj, h.deps.Checkout, testSite,
		testOrderRetry, slog.New(slog.DiscardHandler))
	ctx := t.Context()
	placed := checkout.NewSession("demostore", "shop.test",
		checkout.ProtocolUCP, fixtureCartID)
	placed.OrderUUID = fixtureOrderUUID
	placed.Status = checkout.StatusCompleted
	canceled := checkout.NewSession("demostore", "shop.test",
		checkout.ProtocolUCP, fixtureCartID)
	canceled.Status = checkout.StatusCanceled
	unknown := checkout.NewSession("demostore", "shop.test",
		checkout.ProtocolUCP, fixtureCartID)
	unknown.Status = checkout.StatusCompleteInProgress
	unknown.Unconfirmed = true
	notReady := checkout.NewSession("demostore", "shop.test",
		checkout.ProtocolUCP, fixtureCartID)
	shortfall := &django.StockShortfall{}
	shortfall.FailedItems = append(shortfall.FailedItems, struct {
		ProductID   int64  `json:"productId"`
		ProductName string `json:"productName"`
		Available   int    `json:"available"`
		Requested   int    `json:"requested"`
	}{ProductID: 7, ProductName: "Μπλούζα", Available: 1, Requested: 3})

	for name, tc := range map[string]struct {
		err   error
		code  string
		hints []string
	}{
		"an outcome never recorded": {
			completeErr(placed, checkout.ErrOrderOutcomeUnknown),
			mcptool.CodeOrderOutcomeUnknown,
			[]string{"has not confirmed", "complete_checkout on this " +
				"checkout again", "never places a second",
				"do not start another checkout"}},
		"an outcome never recorded, found by a call": {
			h.closedTo(ctx, unknown, "updated"),
			mcptool.CodeOrderOutcomeUnknown,
			[]string{"has not confirmed", "complete_checkout on this " +
				"checkout again"}},
		"a payment link not made": {
			completeErr(placed, checkout.ErrPaymentSessionFailed),
			mcptool.CodePaymentLinkFailed,
			[]string{"order " + fixtureOrderUUID + " is placed",
				"call complete_checkout again", "do not start a new checkout"}},
		"an order placed, its checkout not saved": {
			completeErr(placed, checkout.ErrSessionNotSaved),
			mcptool.CodeOrderPlaced,
			[]string{"orderUuid " + fixtureOrderUUID,
				"do not complete again or start a new checkout"}},
		"an order placed, its checkout not rendered": {
			unrenderedErr(placed), mcptool.CodeOrderPlaced,
			[]string{"order " + fixtureOrderUUID + " is placed",
				"do not start a new checkout"}},
		"not enough stock": {
			completeErr(placed, shortfall), mcptool.CodeOutOfStock,
			[]string{"Μπλούζα (product 7): requested 3, only 1 available"}},
		"not ready": {
			completeErr(notReady, checkout.ErrNotReady),
			mcptool.CodeMissing,
			[]string{"not ready to complete", checkout.NeedBuyer.String(),
				"update_checkout"}},
		"no payment method": {
			errNoPaymentMethod, mcptool.CodeMissing,
			[]string{"holds no payment method"}},
		"a pay way the delivery is not offered": {
			completeErr(placed, checkout.ErrPayWayUnavailable),
			mcptool.CodeUnsupported,
			[]string{"not offered for this delivery"}},
		"a pay way the surface cannot finish": {
			completeErr(placed, checkout.ErrPaymentMethodUnsupported),
			mcptool.CodeUnsupported,
			[]string{"get_checkout_link"}},
		"a canceled checkout": {
			h.closedTo(ctx, canceled, "updated"), mcptool.CodeCheckoutClosed,
			[]string{"is canceled and cannot be updated"}},
		"a checkout that placed its order": {
			h.closedTo(ctx, placed, "canceled"), mcptool.CodeCheckoutClosed,
			[]string{"already placed order " + fixtureOrderUUID,
				"get_order or track_order"}},
		"no id": {errIDRequired, mcptool.CodeInvalid,
			[]string{"id is required"}},
		"no profile": {(&MetaIn{}).validate(false), mcptool.CodeInvalid,
			[]string{"meta.ucp-agent.profile is required"}},
		"no idempotency key": {
			(&MetaIn{UCPAgent: &UCPAgentIn{Profile: "https://p.test"}}).
				validate(true),
			mcptool.CodeInvalid,
			[]string{"meta.idempotency-key is required"}},
	} {
		t.Run(name, func(t *testing.T) {
			te := codeOf(t, tc.err, tc.code)
			for _, hint := range tc.hints {
				assert.Contains(t, te.Hint, hint)
			}
		})
	}

	// Through the tools themselves, before the store is asked.
	ctx = platformCtx(t)
	_, _, err := h.getCheckout(ctx, nil, GetCheckoutIn{Meta: ucpMeta(t, "")})
	assert.Equal(t, "id is required",
		codeOf(t, err, mcptool.CodeInvalid).Hint)
	_, _, err = h.createCheckout(ctx, nil, CreateCheckoutIn{
		Meta: ucpMeta(t, "")})
	assert.Contains(t, codeOf(t, err, mcptool.CodeInvalid).Hint,
		"checkout.line_items is required")
}

// update_checkout settles every lookup and refusal before its first cart
// write, and reads the stored checkout once more just ahead of it. Its
// body is run here without the lock (that needs a Redis) on a checkout
// built in the test, which the store does not hold: the read
// ahead of the cart writes finds it changed and ends an update that got
// that far:
// whichever step ends the update, no cart call has been made.
func TestUpdateCheckoutSettlesEverythingBeforeTheFirstCartWrite(
	t *testing.T,
) {
	lines := []UCPLineItemReqIn{{Item: UCPItemIn{ID: "2"}, Quantity: 1}}
	for name, tc := range map[string]struct {
		session  checkout.Session
		checkout UCPCheckoutIn
		django   func(t *testing.T) (*django.Client, func() []string)
		want     string
		// reaches is a call the update must have made: the step that
		// ended it, so the refusal is that step's and no other's.
		reaches string
	}{
		"a payment the store does not offer": {
			checkout: UCPCheckoutIn{LineItems: lines,
				UCPPaymentChoice: UCPPaymentChoice{Payment: &UCPPaymentIn{
					Instruments: []UCPInstrumentIn{{
						HandlerID: ucp.HandlerID, Type: "card",
					}},
				}}},
			django: ucpDjango,
			want:   "not available",
		},
		"a pay-way lookup that fails": {
			session:  checkout.Session{PayWayID: 7},
			checkout: UCPCheckoutIn{LineItems: lines},
			django:   downDjango,
			want:     "unavailable",
			reaches:  "GET /api/v1/pay_way",
		},
		"every lookup settled": {
			checkout: UCPCheckoutIn{LineItems: lines},
			django:   ucpDjango,
			want:     errCheckoutChanged.Error(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			dj, calls := tc.django(t)
			h := ucpHandlers(t, dj)
			ctx := platformCtx(t)

			tc.session.Status = checkout.StatusReadyForComplete
			_, _, err := h.negotiated(ctx, ucpMeta(t, ""), false,
				func(tn *tenant.Tenant, neg ucp.Negotiated) (
					*mcp.CallToolResult, CheckoutResult, error,
				) {
					return h.updateSession(ctx, tn, &tc.session,
						UpdateCheckoutIn{Checkout: tc.checkout}, neg).answer()
				})

			require.ErrorContains(t, err, tc.want)
			made := calls()
			for _, call := range made {
				assert.NotContains(t, call, "/api/v1/cart")
			}
			if tc.reaches != "" {
				assert.Contains(t, made, tc.reaches)
			}
		})
	}
}

// downDjango answers every call with a 503 and records the calls.
func downDjango(t *testing.T) (*django.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls = append(calls, r.Method+" "+r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
			time.Second, slog.New(slog.DiscardHandler), nil),
		func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), calls...)
		}
}

// A first completion whose payment link Django refuses for good placed
// its order: it is answered closed, naming the order, never as an order
// placement that failed.
func TestACompletionWhoseOrderCanNoLongerBePaidIsNotAFailedOrder(t *testing.T) {
	s := &checkout.Session{ID: "c", OrderUUID: "o",
		Status: checkout.StatusRequiresEscalation}
	err := completeErr(s, fmt.Errorf("%w: %w",
		checkout.ErrPaymentWindowClosed, errors.New("django: 400")))

	te, ok := errors.AsType[*mcptool.ToolError](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, mcptool.CodeCheckoutClosed, te.Code)
	assert.Contains(t, te.Hint, "order o can no longer be paid")
	assert.Contains(t, te.Hint, "start another checkout: read it")
}
