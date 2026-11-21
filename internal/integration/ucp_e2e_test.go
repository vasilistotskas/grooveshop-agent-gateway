//go:build integration

package integration

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django/djangotest"
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

// newProfiles resolves platform profiles the way ENV=test does: loopback
// http allowed, everything else per the spec's fetch rules.
func newProfiles(t *testing.T) *ucp.ProfileResolver {
	t.Helper()
	p, err := ucp.NewProfileResolver(true, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	return p
}

// servePlatformProfile hosts the recorded platform profile fixture with
// its order webhook pointed at webhookURL; edit rewrites it first.
func servePlatformProfile(
	t *testing.T, webhookURL string, edit func(string) string,
) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata",
		"fixtures", "ucp", "platform_profile.json"))
	require.NoError(t, err)
	body := strings.ReplaceAll(string(raw), "{{webhook_url}}", webhookURL)
	if edit != nil {
		body = edit(body)
	}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
	t.Cleanup(srv.Close)
	return srv.URL + "/profile.json"
}

// receivedWebhook is one order webhook as a platform receives it.
type receivedWebhook struct {
	method, host, path string
	header             http.Header
	body               []byte
}

// webhookSink records signed order webhooks a platform would receive,
// and answers each with status (200 unless set).
type webhookSink struct {
	mu       sync.Mutex
	hooks    []receivedWebhook
	received chan struct{}
	status   atomic.Int32
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
		if status := s.status.Load(); status != 0 {
			w.WriteHeader(int(status))
			return
		}
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
	// UCP requires an order webhook's signature to cover both, and
	// platforms to reject one that omits either.
	for _, name := range []string{`"webhook-id"`, `"webhook-timestamp"`} {
		assert.Contains(t, strings.Fields(m[1]), name, "covered components")
	}
	var base strings.Builder
	for quoted := range strings.FieldsSeq(m[1]) {
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

// checkoutFaults injects upstream failures into the checkout fake.
type checkoutFaults struct {
	// paymentLink makes Viva's payment-session creation fail: the order
	// exists, its payment link does not. paymentLinks counts the links
	// asked for.
	paymentLink  atomic.Bool
	paymentLinks atomic.Int32
	// orderPaid makes an online order answered to a repeated creation
	// read as paid.
	orderPaid atomic.Bool
	// orderCreate makes order creation answer 502 — an outcome the
	// gateway cannot know.
	orderCreate  atomic.Bool
	orderCreates atomic.Int32
	// orderRefuse makes order creation answer 400: nothing is created.
	orderRefuse atomic.Bool
	// orderThrottle makes order creation answer Django's 429: nothing is
	// created.
	orderThrottle atomic.Bool
	// orderUnpaid makes order creation answer Django's 400 for a cart
	// whose earlier order still awaits its online payment: nothing is
	// created.
	orderUnpaid atomic.Bool
	// orderConflict makes order creation answer a 409 that is not a
	// stock shortfall: nothing is created.
	orderConflict atomic.Bool
	// orderAnswerLost loses the answers of that many accepted order
	// creations: the order is placed, and answered 502, as a proxy does
	// for a connection Django dropped after its commit.
	orderAnswerLost atomic.Int32
	// placedBy holds the order each checkout placed (agentCheckout), a
	// fakeOrder. Django answers a repeat with it before its view judges
	// anything else (order/views/order.py, OrderViewSet.create), though
	// after its throttles, so ordersPlaced counts each checkout's order
	// once.
	placedBy sync.Map
	// placing holds a *sync.Mutex per checkout, held from the lookup in
	// placedBy to the order's store there: two creations of one checkout
	// place one order, as Django's unique index on the checkout keeps
	// it.
	placing sync.Map
	// orderHold runs inside an accepted order creation, before it
	// answers.
	orderHold atomic.Pointer[func(*http.Request)]
	// lastOrder is the body of the last order creation that was accepted.
	lastOrder atomic.Pointer[map[string]any]
	// stockShort makes the stock reservation answer a 409 shortfall.
	stockShort atomic.Bool
	// cartDeletedOnOrder makes the cart go with the first order, as it
	// does in Django for a pay way that is not settled online: asked for
	// it, Django answers a new guest cart, and refuses to order from it.
	cartDeletedOnOrder atomic.Bool
	// ordersPlaced counts the order creations that were accepted.
	ordersPlaced atomic.Int32
	// before runs ahead of every request the fake receives.
	before atomic.Pointer[func(*http.Request)]
	// regionLookup makes the country's region list answer 502.
	regionLookup atomic.Bool
	// payWayLookup makes the pay-way list answer 502.
	payWayLookup atomic.Bool
	// payWayDetail makes reading cash on delivery (pay way 1) answer 404.
	payWayDetail atomic.Bool
	// payWayReads counts the reads of the pay-way list; from the read
	// numbered codOnlineFrom on (when set), cash on delivery (pay way 1)
	// reads as a Viva pay way settled online.
	payWayReads   atomic.Int32
	codOnlineFrom atomic.Int32
	// codServedOnline: a wrapper serves cash on delivery (pay way 1) as
	// settled online, so the order placed with it is paid online.
	codServedOnline atomic.Bool
	// codFee makes cash on delivery (pay way 1) read without its free
	// threshold, so its 4.50 fee applies to the fixture cart.
	codFee atomic.Bool
	// codNotDelivered makes the pay-way list for a delivery (one that
	// names a carrier) leave out cash on delivery, as Django does for a
	// carrier that collects no cash.
	codNotDelivered atomic.Bool
	// orderRead makes reading a placed order answer 502.
	orderRead atomic.Bool
	// orderGone makes reading a placed order answer 404: Django no
	// longer has it.
	orderGone atomic.Bool
	// orderCanceled makes a placed order read as canceled.
	orderCanceled atomic.Bool
	// orderOnline is set by an order created with a pay way settled
	// online: reading it then shows it awaiting its payment, its link
	// payable for 30 minutes more and decided 20 after that.
	orderOnline atomic.Bool
	// holdPassed makes an online order read with its payment hold
	// already over.
	holdPassed atomic.Bool
	// paymentProcessing makes an online order read as its payment being
	// processed, which nothing decides; orderUndecided as one nothing
	// decides with its payment pending.
	paymentProcessing atomic.Bool
	orderUndecided    atomic.Bool
	// orderAwaitsReview makes an online order read as a charge that did
	// not match its total, waiting for the store.
	orderAwaitsReview atomic.Bool
	// orderCreatedAt is when the placed order reads as created; unset,
	// the fixture's time, which is long past.
	orderCreatedAt atomic.Pointer[time.Time]
	// lineShort makes a cart line write answer 400 insufficient_stock:
	// the stock went to an order.
	lineShort atomic.Bool
	// cartWrites counts the writes the cart received: its lines and its
	// coupon.
	cartWrites atomic.Int32
	// promotions makes the store take discount codes. Set it before the
	// gateway first resolves the store: the answer is cached.
	promotions atomic.Bool
	// couponWrite makes applying a coupon answer 502.
	couponWrite atomic.Bool
	// couponHeld is the coupon the cart holds, as Django keeps it: an
	// applied coupon is on every later read of the cart until it is
	// removed.
	couponHeld atomic.Bool
	// cartCreates counts the carts created: a cart read naming none.
	cartCreates atomic.Int32
	// namedCartReads counts the reads of a named cart; from the read
	// numbered namedCartReadsFailFrom on (when set), they answer 502.
	namedCartReads         atomic.Int32
	namedCartReadsFailFrom atomic.Int32
	// quoteDown makes the checkout quote answer 502; quotes counts the
	// quotes asked.
	quoteDown atomic.Bool
	quotes    atomic.Int32
	// totalChanged makes order creation answer Django's 409
	// total_changed: the amount due is no longer the expected total.
	totalChanged atomic.Bool
	// lineSwapped makes the cart's line another product at the same
	// price, as Django quotes it: the cart an order is made of is not
	// the one a checkout showed, at the same total. swapOnReserve sets
	// it when stock is reserved: a cart write that lands under a running
	// completion, after its quotes and before its order.
	lineSwapped   atomic.Bool
	swapOnReserve atomic.Bool
	// agentProtocol is the attribution the last order create carried.
	agentProtocol atomic.Value
	// carts are the carts the fake made.
	carts *fakeCarts
}

// endsInHouseNumber reports whether a street line ends in a house number
// Django can split from it: its last word starts with a digit.
func endsInHouseNumber(line string) bool {
	words := strings.Fields(line)
	return len(words) > 1 && unicode.IsDigit([]rune(words[len(words)-1])[0])
}

// knownPickupPoints are the points the fake's catalogue holds, by
// pickupPointOf, and the country each is in.
var knownPickupPoints = map[string]string{
	"boxnow 25": "GR", "acs AAT 502": "GR",
}

// pickupPointOf names an order's pickup point by its carrier's fields.
func pickupPointOf(order map[string]any) string {
	switch order["shippingProviderCode"] {
	case django.ShippingProviderBoxNow:
		return fmt.Sprintf("boxnow %v", order["boxnowLockerId"])
	case django.ShippingProviderACS:
		return fmt.Sprintf("acs %v %v", order["acsStationExternalId"],
			order["acsStationBranch"])
	}
	return ""
}

// fakeCheckoutDjango extends the shared fake with the order-placement
// endpoints the checkout flow drives.
func fakeCheckoutDjango(t *testing.T, faults *checkoutFaults) http.Handler {
	t.Helper()
	outer := http.NewServeMux()
	inner := fakeDjangoMux(t)
	carts := &fakeCarts{made: map[string]bool{}}
	faults.carts = carts
	outer.HandleFunc("GET /api/v1/agent/internal/order/{uuid}",
		func(w http.ResponseWriter, r *http.Request) {
			if faults.orderRead.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			if faults.orderGone.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(
					`{"detail": "Not found.", "code": "not_found"}`))
				return
			}
			createdAt := faults.orderCreatedAt.Load()
			canceled := faults.orderCanceled.Load()
			online := faults.orderOnline.Load()
			if createdAt == nil && !canceled && !online {
				inner.ServeHTTP(w, r)
				return
			}
			raw, err := os.ReadFile(fixturePath("order_by_uuid.json"))
			var order map[string]any
			if !assert.NoError(t, err) ||
				!assert.NoError(t, json.Unmarshal(raw, &order)) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if createdAt != nil {
				order["createdAt"] = createdAt.Format(time.RFC3339Nano)
			}
			if online {
				order["status"], order["paymentStatus"] = "PENDING", "PENDING"
				now := time.Now().UTC()
				hold := now.Add(30 * time.Minute)
				if faults.holdPassed.Load() {
					hold = now.Add(-time.Minute)
				}
				order["paymentHoldExpiresAt"] = hold.Format(time.RFC3339)
				order["paymentDecidedBy"] = now.Add(
					50 * time.Minute).Format(time.RFC3339)
				if faults.paymentProcessing.Load() {
					order["paymentStatus"] = "PROCESSING"
				}
				if faults.paymentProcessing.Load() ||
					faults.orderUndecided.Load() ||
					faults.orderAwaitsReview.Load() {
					order["paymentDecidedBy"] = nil
				}
				order["paymentAwaitsReview"] = faults.orderAwaitsReview.Load()
			}
			if canceled {
				order["status"] = "CANCELED"
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(order)
		})
	outer.HandleFunc("GET /api/v1/cart",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Cart-Id") == "" {
				faults.cartCreates.Add(1)
			} else {
				n := faults.namedCartReads.Add(1)
				from := faults.namedCartReadsFailFrom.Load()
				if from > 0 && n >= from {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
			}
			if faults.cartDeletedOnOrder.Load() &&
				faults.ordersPlaced.Load() > 0 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"uuid": "` + uuid.NewString() +
					`", "items": []}`))
				return
			}
			if faults.couponHeld.Load() {
				serveFixture(t, w, "cart_with_coupon.json")
				return
			}
			inner.ServeHTTP(w, r)
		})
	// quote answers a quote as Django does, from the cart and pay ways the
	// faults leave; order creation compares its expected total with it.
	quote := func(w http.ResponseWriter, r *http.Request) {
		if faults.cartDeletedOnOrder.Load() &&
			faults.ordersPlaced.Load() > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail": "Cart not found", ` +
				`"code": "cart_not_found"}`))
			return
		}
		read := func(name string) []byte {
			raw, err := os.ReadFile(fixturePath(name))
			assert.NoError(t, err)
			return raw
		}
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		var req django.QuoteRequest
		assert.NoError(t, json.Unmarshal(body, &req))
		r.Body = io.NopCloser(bytes.NewReader(body))
		cart := "cart_with_items.json"
		if faults.couponHeld.Load() {
			cart = "cart_with_coupon.json"
		}
		lines := read(cart)
		if faults.lineSwapped.Load() {
			lines = bytes.Replace(lines, []byte(`"product":{"id":1,`),
				[]byte(`"product":{"id":2,`), 1)
		}
		cod := read("pay_way_1.json")
		if faults.codFee.Load() {
			cod = bytes.Replace(cod, []byte(`"freeThreshold": 100.0`),
				[]byte(`"freeThreshold": 0.0`), 1)
		}
		ways := map[int64][]byte{2: read("pay_way_2.json")}
		// As the pay-way list: cash on delivery is offered for no
		// delivery that names a carrier.
		if !faults.codNotDelivered.Load() ||
			req.ShippingProviderCode == "" {
			ways[1] = cod
		}
		djangotest.Store{
			Cart: lines, PayWays: ways,
			ShippingOptions: read("shipping_options.json"),
		}.Quote(w, r)
	}
	// The catalogue lists a pickup point is read from, by Django's exact
	// filters.
	outer.HandleFunc("GET /api/v1/shipping/boxnow/lockers",
		func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			assert.NotEmpty(t, q.Get("externalId"))
			assert.NotEmpty(t, q.Get("countryCode"))
			serveCatalogue(t, w, "boxnow_lockers_search.json",
				map[string]string{"externalId": q.Get("externalId"),
					"countryCode": q.Get("countryCode")})
		})
	outer.HandleFunc("GET /api/v1/shipping/acs/stations",
		func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			assert.NotEmpty(t, q.Get("externalId"))
			assert.True(t, q.Has("branch"), "the branch, blank or not")
			assert.NotEmpty(t, q.Get("countryCode"))
			serveCatalogue(t, w, "acs_stations_search.json",
				map[string]string{"externalId": q.Get("externalId"),
					"branchCode":  q.Get("branch"),
					"countryCode": q.Get("countryCode")})
		})
	outer.HandleFunc("POST /api/v1/cart/quote",
		func(w http.ResponseWriter, r *http.Request) {
			faults.quotes.Add(1)
			if faults.quoteDown.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			quote(w, r)
		})
	// amountDue is Django's quote of an order with these choices now: its
	// expected total and cart digest, or its refusal of a choice.
	amountDue := func(order map[string]any) *httptest.ResponseRecorder {
		// A map of decoded JSON values always encodes.
		choices, _ := json.Marshal(map[string]any{
			"payWayId": order["payWayId"], "shippingKind": order["shippingKind"],
			"shippingProviderCode": order["shippingProviderCode"],
			"countryId":            order["countryId"], "regionId": order["regionId"],
			"email":                order["email"],
			"acsStationExternalId": order["acsStationExternalId"],
			"acsStationBranch":     order["acsStationBranch"],
			"boxnowLockerId":       order["boxnowLockerId"],
		})
		rec := httptest.NewRecorder()
		quote(rec, httptest.NewRequest(http.MethodPost, "/api/v1/cart/quote",
			bytes.NewReader(choices)))
		return rec
	}
	outer.HandleFunc("GET /api/v1/region/GR/get_regions_by_country_alpha_2",
		func(w http.ResponseWriter, r *http.Request) {
			if faults.regionLookup.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			inner.ServeHTTP(w, r)
		})
	outer.HandleFunc("GET /api/v1/pay_way",
		func(w http.ResponseWriter, r *http.Request) {
			if faults.payWayLookup.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			read := faults.payWayReads.Add(1)
			if faults.codNotDelivered.Load() &&
				r.URL.Query().Get("shippingProviderCode") != "" {
				raw, err := os.ReadFile(fixturePath("pay_way.json"))
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				var page map[string]any
				if !assert.NoError(t, json.Unmarshal(raw, &page)) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				var kept []any
				for _, pw := range page["results"].([]any) {
					if pw.(map[string]any)["providerCode"] !=
						django.ProviderCashOnDelivery {
						kept = append(kept, pw)
					}
				}
				page["results"], page["count"] = kept, len(kept)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(page)
				return
			}
			if from := faults.codOnlineFrom.Load(); from > 0 && read >= from {
				raw, err := os.ReadFile(fixturePath("pay_way.json"))
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				const cod = `"providerCode":"cash_on_delivery",` +
					`"settlement":"courier_cash"`
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(strings.Replace(string(raw), cod,
					`"providerCode":"viva_wallet","settlement":"online"`, 1)))
				return
			}
			inner.ServeHTTP(w, r)
		})
	outer.HandleFunc("GET /api/v1/pay_way/1",
		func(w http.ResponseWriter, r *http.Request) {
			if faults.payWayDetail.Load() {
				// Switched off, or its provider unconfigured: Django
				// hides it (PayWayService.shopper_facing).
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if !faults.codFee.Load() {
				inner.ServeHTTP(w, r)
				return
			}
			raw, err := os.ReadFile(fixturePath("pay_way_1.json"))
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			const threshold = `"freeThreshold": 100.0`
			if !assert.Contains(t, string(raw), threshold) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.Replace(string(raw), threshold,
				`"freeThreshold": 0.0`, 1)))
		})
	for _, write := range []string{
		"POST /api/v1/cart/item", "PUT /api/v1/cart/item/{id}",
		"DELETE /api/v1/cart/item/{id}",
	} {
		outer.HandleFunc(write,
			func(w http.ResponseWriter, r *http.Request) {
				faults.cartWrites.Add(1)
				refuse := func(status int, body string) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = w.Write([]byte(body))
				}
				switch {
				// The line went with the order's cart: Django answers a
				// changed line 404 and a deleted one 403
				// (src/cart/views/item.py get_object, destroy).
				case faults.cartDeletedOnOrder.Load() &&
					faults.ordersPlaced.Load() > 0 &&
					r.Method == http.MethodPut:
					refuse(http.StatusNotFound, `{"detail": "No CartItem `+
						`matches the given query.", "code": "not_found"}`)
				case faults.cartDeletedOnOrder.Load() &&
					faults.ordersPlaced.Load() > 0 &&
					r.Method == http.MethodDelete:
					refuse(http.StatusForbidden, `{"detail": "You do not `+
						`have permission to delete this cart item.", `+
						`"code": "cart_item_forbidden"}`)
				// The stock went to an order (src/cart/serializers/item.py
				// validate, through core/api/exception_handler.py).
				case faults.lineShort.Load() &&
					r.Method != http.MethodDelete:
					const msg = "Not enough stock for 'Ζυμαρικά'. " +
						"Available: 0, Requested: 3"
					refuse(http.StatusBadRequest, `{"non_field_errors": ["`+
						msg+`"], "code": "insufficient_stock", "fields": `+
						`{"non_field_errors": [{"message": "`+msg+
						`", "code": "insufficient_stock"}]}}`)
				default:
					inner.ServeHTTP(w, r)
				}
			})
	}
	outer.HandleFunc("POST /api/v1/cart/coupon",
		func(w http.ResponseWriter, _ *http.Request) {
			if faults.couponWrite.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			faults.cartWrites.Add(1)
			faults.couponHeld.Store(true)
			serveFixture(t, w, "cart_with_coupon.json")
		})
	outer.HandleFunc("GET /api/v1/tenant/resolve",
		func(w http.ResponseWriter, r *http.Request) {
			if !faults.promotions.Load() {
				inner.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			inner.ServeHTTP(rec, r)
			maps.Copy(w.Header(), rec.Header())
			w.WriteHeader(rec.Code)
			_, _ = w.Write(bytes.Replace(rec.Body.Bytes(),
				[]byte(`"promotions": false`),
				[]byte(`"promotions": true`), 1))
		})
	outer.HandleFunc("DELETE /api/v1/cart/coupon",
		func(w http.ResponseWriter, _ *http.Request) {
			faults.cartWrites.Add(1)
			faults.couponHeld.Store(false)
			serveFixture(t, w, "cart_with_items.json")
		})
	outer.HandleFunc("POST /api/v1/cart/reserve-stock",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if faults.cartDeletedOnOrder.Load() &&
				faults.ordersPlaced.Load() > 0 {
				// The cart went with its order, so Django reserves for
				// the new, empty cart it makes for the id
				// (src/cart/views/cart.py reserve_stock).
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"detail": "Cart is empty. Cannot ` +
					`reserve stock for empty cart.", "code": "cart_empty"}`))
				return
			}
			if faults.swapOnReserve.Load() {
				faults.lineSwapped.Store(true)
			}
			if faults.stockShort.Load() {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"detail": "Not enough stock.",
					"failedItems": [{"productId": 1, "productName":
					"Ζυμαρικά", "available": 1, "requested": 2}]}`))
				return
			}
			_, _ = w.Write([]byte(
				`{"reservationIds": [77], "message": "reserved"}`))
		})
	outer.HandleFunc("POST /api/v1/order",
		func(w http.ResponseWriter, r *http.Request) {
			faults.orderCreates.Add(1)
			if faults.orderCreate.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			// DRF throttles before the view (APIView.initial).
			if faults.orderThrottle.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"detail": "Request was throttled."}`))
				return
			}
			raw, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			var body map[string]any
			assert.NoError(t, json.Unmarshal(raw, &body))
			// Every order names its checkout, which places one order.
			checkoutID, _ := body["agentCheckout"].(string)
			assert.NotEmpty(t, checkoutID)
			placing, _ := faults.placing.LoadOrStore(checkoutID,
				&sync.Mutex{})
			placing.(*sync.Mutex).Lock()
			defer placing.(*sync.Mutex).Unlock()
			if placed, ok := faults.placedBy.Load(checkoutID); ok {
				servePlacedOrder(t, w, faults, checkoutID, placed.(fakeOrder))
				return
			}
			if faults.orderRefuse.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(
					`{"detail": "The cart can no longer be ordered."}`))
				return
			}
			if faults.orderUnpaid.Load() {
				// order/views/order.py, CartHasUnpaidOrderError, through
				// core/api/exception_handler.py.
				raw, err := os.ReadFile(
					fixturePath("order_cart_has_unpaid_order.json"))
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write(raw)
				return
			}
			if faults.totalChanged.Load() {
				raw, err := os.ReadFile(
					fixturePath("order_total_changed.json"))
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write(raw)
				return
			}
			if faults.orderConflict.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(
					`{"detail": "The cart changed while it was ordered."}`))
				return
			}
			if faults.cartDeletedOnOrder.Load() &&
				faults.ordersPlaced.Load() > 0 {
				// Django's answer for a cart that no longer exists:
				// order/views/order.py _get_cart_and_user, through
				// core/api/exception_handler.py.
				const msg = "Cart not found. Please add items to cart " +
					"before checkout."
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"cart": ["` + msg + `"], ` +
					`"code": "cart_not_found", "fields": {"cart": [` +
					`{"message": "` + msg + `", "code": "cart_not_found"}]}}`))
				return
			}
			// The cart rides the header, never the body.
			assert.True(t, carts.known(r.Header.Get("X-Cart-Id")),
				"the order names a cart the store never made")
			assert.NotEmpty(t, body["payWayId"])
			assert.NotEmpty(t, body["email"])
			pickup := body["shippingKind"] == checkout.FulfillmentPickupPoint
			if !pickup {
				// Django refuses a home delivery without a region when
				// the country has any, and Greece does.
				assert.Equal(t, "GR-14", body["regionId"])
			}
			// Every agent checkout linked the terms and privacy policy,
			// so every order it places accepts them.
			assert.Equal(t, true, body["termsAccepted"])
			// The street is one line, which Django splits, refusing one
			// that ends in no house number (core/validators/address.py
			// split_street_line, through the order serializer).
			assert.NotContains(t, body, "streetNumber")
			// A pickup order carries no address, and is delivered to a
			// point the catalogue holds (order/serializers/order.py
			// _deliver_to_pickup_point).
			if pickup {
				for _, field := range []string{"street", "city", "zipcode"} {
					assert.NotContains(t, body, field)
				}
				refusal := ""
				switch country, known := knownPickupPoints[pickupPointOf(body)]; {
				case !known:
					refusal = "order_pickup_point_unknown.json"
				case country != body["countryId"]:
					refusal = "order_pickup_point_in_another_country.json"
				}
				if refusal != "" {
					raw, err := os.ReadFile(fixturePath(refusal))
					if !assert.NoError(t, err) {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write(raw)
					return
				}
			} else if street, _ := body["street"].(string); !endsInHouseNumber(street) {
				raw, err := os.ReadFile(
					fixturePath("order_street_number_missing.json"))
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write(raw)
				return
			}
			// It is placed only from the cart quoted, and at the amount
			// due now, as Django places it, the cart checked first
			// (order/services.py, OrderCartChangedError and
			// OrderTotalChangedError).
			assert.NotEmpty(t, body["expectedTotal"])
			assert.NotEmpty(t, body["expectedCartDigest"])
			due := amountDue(body)
			if due.Code != http.StatusOK {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(due.Code)
				_, _ = w.Write(due.Body.Bytes())
				return
			}
			var now map[string]any
			assert.NoError(t, json.Unmarshal(due.Body.Bytes(), &now))
			if now["cartDigest"] != body["expectedCartDigest"] {
				raw, err := os.ReadFile(
					fixturePath("order_cart_changed.json"))
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write(raw)
				return
			}
			if !assert.ObjectsAreEqual(now["expectedTotal"],
				body["expectedTotal"]) {
				raw, err := os.ReadFile(
					fixturePath("order_total_changed.json"))
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write(raw)
				return
			}
			if attribution, ok := body["attribution"].(map[string]any); ok {
				faults.agentProtocol.Store(attribution["agentProtocol"])
			}
			faults.lastOrder.Store(&body)
			online := body["payWayId"] != float64(1) ||
				faults.codOnlineFrom.Load() > 0 ||
				faults.codServedOnline.Load()
			faults.orderOnline.Store(online)
			if hold := faults.orderHold.Load(); hold != nil {
				(*hold)(r)
			}
			faults.ordersPlaced.Add(1)
			placed := fakeOrder{online: online, at: time.Now().UTC()}
			if at := faults.orderCreatedAt.Load(); at != nil {
				placed.at = *at
			}
			faults.placedBy.Store(checkoutID, placed)
			if faults.orderAnswerLost.Add(-1) >= 0 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			faults.orderAnswerLost.Store(0)
			servePlacedOrder(t, w, faults, checkoutID, placed)
		})
	outer.HandleFunc("POST /api/v1/order/684/create_checkout_session",
		func(w http.ResponseWriter, r *http.Request) {
			faults.paymentLinks.Add(1)
			// Guest authorization rides ?uuid=.
			assert.Equal(t, fixtureOrderUUID, r.URL.Query().Get("uuid"))
			if faults.paymentLink.Load() {
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
	outer.Handle("/", inner)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if before := faults.before.Load(); before != nil {
			(*before)(r)
		}
		carts.serve(outer, w, r)
	})
}

// fakeOrder is an order the fake placed: whether it is paid online, and
// when it was created.
type fakeOrder struct {
	online bool
	at     time.Time
}

// servePlacedOrder answers an order creation with the fixture's order,
// placed by checkoutID, as Django answers the first creation and every
// repeat (201). An online one awaits its payment, which the faults may
// have moved on (paymentProcessing, orderCanceled), and its hold is
// ahead unless holdPassed. It runs on the fake's handler goroutine: an
// error is reported, never t.FailNow.
func servePlacedOrder(
	t *testing.T, w http.ResponseWriter, faults *checkoutFaults,
	checkoutID string, placed fakeOrder,
) {
	t.Helper()
	raw, err := os.ReadFile(fixturePath("order_by_uuid.json"))
	var order map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &order)
	}
	if err != nil {
		t.Errorf("read the fixture: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	order["agentCheckout"] = checkoutID
	order["createdAt"] = placed.at.Format(time.RFC3339Nano)
	if placed.online {
		now := time.Now().UTC()
		hold := now.Add(30 * time.Minute)
		if faults.holdPassed.Load() {
			hold = now.Add(-time.Minute)
		}
		order["isOnlinePayment"] = true
		order["status"], order["paymentStatus"] = "PENDING", "PENDING"
		order["paymentHoldExpiresAt"] = hold.Format(time.RFC3339)
		order["paymentDecidedBy"] = now.Add(
			50 * time.Minute).Format(time.RFC3339)
		if faults.paymentProcessing.Load() {
			order["paymentStatus"] = django.PaymentStatusProcessing
			order["paymentDecidedBy"] = nil
		}
		if faults.orderCanceled.Load() {
			order["status"] = django.OrderStatusCanceled
		}
		if faults.orderPaid.Load() {
			order["paymentStatus"] = django.PaymentStatusCompleted
			order["paymentHoldExpiresAt"] = nil
			order["paymentDecidedBy"] = nil
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(order)
}

// fakeCarts gives each cart the fake makes an id of its own, as Django
// does. The fixtures all carry fixtureCartID, so an answer about a cart
// made here is rewritten to that cart's id. A cart the fake never made
// is answered with the fixture's cart, which is another cart to its
// caller, as Django's new guest cart is.
type fakeCarts struct {
	mu   sync.Mutex
	made map[string]bool
}

// known reports whether id is the fixture's cart or one made here.
func (c *fakeCarts) known(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return id == fixtureCartID || c.made[id]
}

// mint makes a cart, which reads as the fixture's cart with items.
func (c *fakeCarts) mint() string {
	id := uuid.NewString()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.made[id] = true
	return id
}

func (c *fakeCarts) serve(
	next http.Handler, w http.ResponseWriter, r *http.Request,
) {
	id := r.Header.Get("X-Cart-Id")
	if id == "" && r.Method == http.MethodGet &&
		r.URL.Path == "/api/v1/cart" {
		id = c.mint()
	}
	if id == fixtureCartID || !c.known(id) {
		next.ServeHTTP(w, r)
		return
	}
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	maps.Copy(w.Header(), rec.Header())
	w.WriteHeader(rec.Code)
	_, _ = w.Write(bytes.ReplaceAll(rec.Body.Bytes(),
		[]byte(fixtureCartID), []byte(id)))
}

// ucpUpstreamTimeout is how long the stack's gateway waits for Django.
const ucpUpstreamTimeout = 5 * time.Second

// ucpStack is the full gateway over a fake Django and real Redis.
type ucpStack struct {
	gw  *httptest.Server
	key *ucp.SigningKey
	rdb *redis.Client
	// faults toggles upstream failures.
	faults *checkoutFaults
	// logs is what the gateway logged, from info up.
	logs *logLines
}

// logLines collects a gateway's log output for a test to read.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// with returns the first line that contains every part, or "".
func (l *logLines) with(parts ...string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
lines:
	for line := range strings.SplitSeq(l.buf.String(), "\n") {
		for _, part := range parts {
			if !strings.Contains(line, part) {
				continue lines
			}
		}
		return line
	}
	return ""
}

// startUCPGateway boots the full stack with real Redis, the signing keys
// checked, and the internal events route armed.
func startUCPGateway(t *testing.T) ucpStack {
	t.Helper()
	return startUCPGatewayBehind(t,
		func(h http.Handler) http.Handler { return h })
}

// startUCPGatewayBehind is startUCPGateway with the gateway's handler
// wrapped in front, for a test that watches the requests it serves.
func startUCPGatewayBehind(
	t *testing.T, front func(http.Handler) http.Handler,
) ucpStack {
	t.Helper()
	faults := new(checkoutFaults)
	djangoSrv := httptest.NewServer(fakeCheckoutDjango(t, faults))
	t.Cleanup(djangoSrv.Close)
	rdb := startRedis(t)

	logs := new(logLines)
	log := slog.New(obs.NewContextHandler(slog.NewTextHandler(logs, nil)))
	metrics := obs.NewMetrics()
	cfg := config.Config{
		Env: config.EnvTest,
		// Registers httptest webhook endpoints on 127.0.0.1.
		AllowLocalWebhooks:      true,
		PaymentHandlerEnv:       config.HandlerEnvSandbox,
		DjangoBaseURL:           djangoSrv.URL + "/api/v1",
		DjangoPublicHost:        "api.example.test",
		InternalSecret:          internalSecret,
		AssetsHost:              "assets.platform.test",
		StorefrontDefaultLocale: "el",
		MediaURLTemplate: "https://{assets_host}/media_stream-image/{path}" +
			"/800/800/contain/entropy/transparent/5/80.webp",
		TenantCacheTTL:        time.Minute,
		NegativeCacheTTL:      time.Minute,
		UpstreamTimeout:       ucpUpstreamTimeout,
		OrderCreateAttempts:   testOrderRetry.Attempts,
		OrderCreateRetryDelay: testOrderRetry.Delay,
		RateLimitPerMin:       6000,
		RateLimitBurst:        1000,
		UCPSignatureMaxAge:    5 * time.Minute,
		UCPSignatureClockSkew: time.Minute,
	}
	dj := django.New(cfg.DjangoBaseURL, cfg.DjangoPublicHost, internalSecret,
		cfg.UpstreamTimeout, log, metrics)
	resolver := tenant.NewResolver(dj, rdb,
		cfg.TenantCacheTTL, cfg.NegativeCacheTTL, log, metrics)

	keys := newKeys(t, rdb, testKEK(t))
	// The fixture tenant's key, for signature/KID assertions.
	key, err := keys.ForSchema(t.Context(), "demostore")
	require.NoError(t, err)
	require.NoError(t, keys.AwaitKEK(t.Context(), log))

	handler := server.New(server.Deps{
		Cfg: cfg, Log: log, Metrics: metrics, Redis: rdb,
		Django: dj, Resolver: resolver, Version: "test",
		Keys: keys, Webhooks: ucp.NewWebhooks(keys, true),
		Profiles: newProfiles(t),
	})
	srv := httptest.NewServer(front(handler))
	t.Cleanup(srv.Close)
	return ucpStack{
		gw: srv, key: key, rdb: rdb, faults: faults, logs: logs,
	}
}

// fixtureSoldName is the name order_by_uuid.json's line was sold under;
// its product has been renamed since.
const fixtureSoldName = "Ιταλικά Ζυμαρικά Classic"

// The placed order's totals in order_by_uuid.json, in minor units.
const (
	fixtureOrderSubtotal = 6467
	fixtureOrderFee      = 450
	fixtureOrderDiscount = 500
	fixtureOrderTotal    = 6417
)

// Django deletes the cart with the order, so a completed checkout renders
// from the order. Rendering it from the cart failed, and told the agent
// to retry an order that already existed.
func TestUCPCompletedCheckoutRendersFromTheOrder(t *testing.T) {
	stack := startUCPGateway(t)
	stack.faults.cartDeletedOnOrder.Store(true)
	session := connectMCP(t, stack.gw.URL)
	platform := httptest.NewServer(newWebhookSink().handler())
	t.Cleanup(platform.Close)
	profile := servePlatformProfile(t, platform.URL+"/ucp/orders", nil)
	meta := func(idem string) map[string]any {
		m := map[string]any{"ucp-agent": map[string]any{"profile": profile}}
		if idem != "" {
			m["idempotency-key"] = idem
		}
		return m
	}
	cod := map[string]any{"instruments": []any{map[string]any{
		"handler_id": ucp.HandlerID,
		"type":       ucp.InstrumentCashOnDelivery, "selected": true,
	}}}

	res := callTool(t, session, "create_checkout", map[string]any{
		"meta": meta(""), "cart_id": fixtureCartID,
		"checkout": map[string]any{
			"buyer": map[string]any{
				"first_name": "Μαρία", "last_name": "Παπαδοπούλου",
				"email": "maria@example.test", "phone_number": "+306912345678",
			},
			"fulfillment": homeDelivery(),
			"payment":     cod,
		},
	})
	require.False(t, res.IsError)
	id := structured(t, res)["id"]

	assertRendered := func(res *mcp.CallToolResult) {
		t.Helper()
		require.False(t, res.IsError, "%v", res.Content)
		out := structured(t, res)
		assert.Equal(t, "completed", out["status"])
		assert.Equal(t, fixtureOrderUUID,
			out["order"].(map[string]any)["id"])
		amounts := map[string]float64{}
		for _, row := range out["totals"].([]any) {
			total := row.(map[string]any)
			amounts[total["type"].(string)] = total["amount"].(float64)
		}
		assert.InDelta(t, fixtureOrderSubtotal, amounts["subtotal"], 0)
		assert.InDelta(t, fixtureOrderFee, amounts["fee"], 0)
		assert.InDelta(t, -fixtureOrderDiscount, amounts["discount"], 0)
		assert.InDelta(t, fixtureOrderTotal, amounts["total"], 0)
		lines := out["line_items"].([]any)
		require.Len(t, lines, 1)
		assert.Equal(t, fixtureSoldName,
			lines[0].(map[string]any)["item"].(map[string]any)["title"])
	}

	// The order goes through but cannot be read back: the answer must say it
	// exists, or the agent would order again.
	key := uuid.NewString()
	stack.faults.orderRead.Store(true)
	res = callTool(t, session, "complete_checkout", map[string]any{
		"meta": meta(key), "id": id, "checkout": map[string]any{"payment": cod},
	})
	require.True(t, res.IsError)
	failure := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, failure, "order "+fixtureOrderUUID+" is placed")
	assert.Contains(t, failure, "do not start a new checkout")
	stack.faults.orderRead.Store(false)

	assertRendered(callTool(t, session, "complete_checkout", map[string]any{
		"meta": meta(key), "id": id, "checkout": map[string]any{"payment": cod},
	}))
	assertRendered(callTool(t, session, "get_checkout", map[string]any{
		"meta": meta(""), "id": id,
	}))
	assertRendered(callTool(t, session, "complete_checkout", map[string]any{
		"meta": meta(key), "id": id, "checkout": map[string]any{"payment": cod},
	}))
	assert.EqualValues(t, 1, stack.faults.orderCreates.Load(),
		"a repeated complete places nothing")

	stack.faults.orderRead.Store(true)
	res = callTool(t, session, "get_checkout", map[string]any{
		"meta": meta(""), "id": id,
	})
	require.True(t, res.IsError)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text,
		"order "+fixtureOrderUUID+" is placed")
}

// The region is resolved before the cart is touched: an update that fails
// on it must leave the cart as the stored session knows it.
func TestRegionLookupFailureLeavesTheCartAlone(t *testing.T) {
	stack := startUCPGateway(t)
	platform := httptest.NewServer(newWebhookSink().handler())
	t.Cleanup(platform.Close)
	meta := map[string]any{"ucp-agent": map[string]any{
		"profile": servePlatformProfile(t, platform.URL+"/ucp/orders", nil),
	}}
	greek := homeDelivery()

	t.Run("ucp", func(t *testing.T) {
		session := connectMCP(t, stack.gw.URL)
		res := callTool(t, session, "create_checkout", map[string]any{
			"meta": meta, "cart_id": fixtureCartID,
			"checkout": map[string]any{},
		})
		require.False(t, res.IsError)

		stack.faults.regionLookup.Store(true)
		t.Cleanup(func() { stack.faults.regionLookup.Store(false) })
		writes := stack.faults.cartWrites.Load()
		res = callTool(t, session, "update_checkout", map[string]any{
			"meta": meta, "id": structured(t, res)["id"],
			"checkout": map[string]any{
				"fulfillment": greek,
				"line_items": []any{map[string]any{
					"item": map[string]any{"id": "2"}, "quantity": 1,
				}},
			},
		})
		assert.True(t, res.IsError)
		assert.Equal(t, writes, stack.faults.cartWrites.Load())
	})

	t.Run("acp", func(t *testing.T) {
		stack.faults.regionLookup.Store(false)
		base := stack.gw.URL + "/acp/checkout_sessions"
		resp, created := acpCall(t, http.MethodPost, base, map[string]any{
			"line_items":   []map[string]any{{"id": "1", "quantity": 1}},
			"currency":     "EUR",
			"capabilities": map[string]any{},
		}, uuid.NewString())
		require.Equal(t, http.StatusCreated, resp.StatusCode)

		stack.faults.regionLookup.Store(true)
		writes := stack.faults.cartWrites.Load()
		resp, _ = acpCall(t, http.MethodPost,
			base+"/"+created["id"].(string), map[string]any{
				"line_items": []map[string]any{{"id": "2", "quantity": 1}},
				"fulfillment_details": map[string]any{
					"name": "Μαρία Παπαδοπούλου",
					"address": map[string]any{
						"name":     "Μαρία Παπαδοπούλου",
						"line_one": "Πανεπιστημίου 12", "city": "Αθήνα",
						"state": "GR-14", "country": "GR",
						"postal_code": "10431",
					},
				},
			}, uuid.NewString())
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, writes, stack.faults.cartWrites.Load())
	})
}

// A payment the store refuses is refused before the cart is written: an
// update leaves the cart as the stored session knows it, with the error
// the agent reads, and a create leaves no cart behind.
func TestRefusedPaymentLeavesTheCartAlone(t *testing.T) {
	stack := startUCPGateway(t)
	platform := httptest.NewServer(newWebhookSink().handler())
	t.Cleanup(platform.Close)
	session := connectMCP(t, stack.gw.URL)
	meta := map[string]any{"ucp-agent": map[string]any{
		"profile": servePlatformProfile(t, platform.URL+"/ucp/orders", nil),
	}}
	refused := map[string]any{
		"line_items": []any{map[string]any{
			"item": map[string]any{"id": "2"}, "quantity": 1,
		}},
		"discounts": map[string]any{"codes": []any{"SAVE10"}},
		"payment": map[string]any{"instruments": []any{map[string]any{
			"handler_id": "com.example.elsewhere",
			"type":       ucp.InstrumentCashOnDelivery,
		}}},
	}
	untouched := func(t *testing.T, res *mcp.CallToolResult,
		writes, creates int32) {
		t.Helper()
		require.True(t, res.IsError)
		assert.Contains(t, res.Content[0].(*mcp.TextContent).Text,
			"unknown payment handler_id")
		assert.Equal(t, writes, stack.faults.cartWrites.Load())
		assert.Equal(t, creates, stack.faults.cartCreates.Load())
	}

	t.Run("update", func(t *testing.T) {
		res := callTool(t, session, "create_checkout", map[string]any{
			"meta": meta, "cart_id": fixtureCartID,
			"checkout": map[string]any{},
		})
		require.False(t, res.IsError)
		writes := stack.faults.cartWrites.Load()
		creates := stack.faults.cartCreates.Load()

		untouched(t, callTool(t, session, "update_checkout", map[string]any{
			"meta": meta, "id": structured(t, res)["id"],
			"checkout": refused,
		}), writes, creates)
	})

	t.Run("create", func(t *testing.T) {
		writes := stack.faults.cartWrites.Load()
		creates := stack.faults.cartCreates.Load()

		untouched(t, callTool(t, session, "create_checkout", map[string]any{
			"meta": meta, "checkout": refused,
		}), writes, creates)
	})
}

// An ACP create settles its lookups and refusals before it makes its
// cart, and an update before it writes the cart: one the store refuses
// there leaves no cart behind, or the cart as it was.
func TestARefusedACPCallLeavesTheCartAlone(t *testing.T) {
	stack := startUCPGateway(t)
	base := stack.gw.URL + "/acp/checkout_sessions"
	create := func(itemID string) map[string]any {
		return map[string]any{
			"line_items":   []map[string]any{{"id": itemID, "quantity": 1}},
			"currency":     "EUR",
			"capabilities": map[string]any{},
			"fulfillment_details": map[string]any{
				"name": "Μαρία Παπαδοπούλου",
				"address": map[string]any{
					"name":     "Μαρία Παπαδοπούλου",
					"line_one": "Πανεπιστημίου 12", "city": "Αθήνα",
					"state": "GR-14", "country": "GR",
					"postal_code": "10431",
				},
			},
		}
	}
	for _, tc := range []struct {
		name   string
		item   string
		fault  *atomic.Bool
		status int
		code   string
	}{
		{"a region lookup that fails", "1", &stack.faults.regionLookup,
			http.StatusServiceUnavailable, "upstream_unavailable"},
		{"an item that is not a product id", "x", nil,
			http.StatusBadRequest, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fault != nil {
				tc.fault.Store(true)
				t.Cleanup(func() { tc.fault.Store(false) })
			}
			writes := stack.faults.cartWrites.Load()
			creates := stack.faults.cartCreates.Load()
			resp, body := acpCall(t, http.MethodPost, base,
				create(tc.item), uuid.NewString())
			require.Equal(t, tc.status, resp.StatusCode, "%v", body)
			assert.Equal(t, tc.code, body["code"])
			assert.Equal(t, creates, stack.faults.cartCreates.Load())
			assert.Equal(t, writes, stack.faults.cartWrites.Load())
		})
	}

	// An update settles them before its first cart write too: a pay-way
	// lookup that fails, which runs once a delivery is chosen, leaves the
	// cart and the stored session as they were.
	t.Run("an update whose pay-way lookup fails", func(t *testing.T) {
		resp, created := acpCall(t, http.MethodPost, base, create("1"),
			uuid.NewString())
		require.Equal(t, http.StatusCreated, resp.StatusCode, "%v", created)
		session := base + "/" + created["id"].(string)
		_, before := acpCall(t, http.MethodGet, session, nil, "")
		update := map[string]any{
			"line_items": []map[string]any{{"id": "2", "quantity": 1}},
			"selected_fulfillment_options": []map[string]any{{
				"type": "shipping", "option_id": "acs:home_delivery",
				"item_ids": []string{"410"},
			}},
		}

		stack.faults.payWayLookup.Store(true)
		t.Cleanup(func() { stack.faults.payWayLookup.Store(false) })
		writes := stack.faults.cartWrites.Load()
		resp, body := acpCall(t, http.MethodPost, session, update,
			uuid.NewString())
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
			"%v", body)
		assert.Equal(t, "upstream_unavailable", body["code"])
		assert.Equal(t, writes, stack.faults.cartWrites.Load())
		stack.faults.payWayLookup.Store(false)
		_, after := acpCall(t, http.MethodGet, session, nil, "")
		assert.Equal(t, before, after)

		resp, body = acpCall(t, http.MethodPost, session, update,
			uuid.NewString())
		require.Equal(t, http.StatusOK, resp.StatusCode, "%v", body)
		assert.NotEqual(t, before, body)
		assert.Greater(t, stack.faults.cartWrites.Load(), writes)
	})

	t.Run("a create the store takes", func(t *testing.T) {
		writes := stack.faults.cartWrites.Load()
		creates := stack.faults.cartCreates.Load()
		resp, body := acpCall(t, http.MethodPost, base, create("1"),
			uuid.NewString())
		require.Equal(t, http.StatusCreated, resp.StatusCode, "%v", body)
		assert.Equal(t, creates+1, stack.faults.cartCreates.Load())
		assert.Greater(t, stack.faults.cartWrites.Load(), writes)
	})
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
	// The platform's profile names its order-event endpoint: that is
	// where webhooks go, never a request member.
	profileURL := servePlatformProfile(t, platform.URL+"/ucp/orders", nil)
	meta := func(idem string) map[string]any {
		m := map[string]any{
			"ucp-agent": map[string]any{"profile": profileURL},
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
	fulfillment := homeDelivery()
	// An update replaces the checkout, so it carries the lines: the
	// minted cart's own, product 1 twice.
	cartLines := []any{map[string]any{
		"item": map[string]any{"id": "1"}, "quantity": 2,
	}}

	t.Run("a country with regions blocks completion until one is chosen",
		func(t *testing.T) {
			noRegion := shipTo("")
			res := callTool(t, session, "create_checkout", map[string]any{
				"meta":    meta(""),
				"cart_id": stack.faults.carts.mint(),
				"checkout": map[string]any{
					"buyer": buyer, "fulfillment": noRegion,
					"payment": codInstrument,
				},
			})
			require.False(t, res.IsError)
			created := structured(t, res)
			assert.Equal(t, "incomplete", created["status"])
			assert.Contains(t, fmt.Sprint(created["messages"]),
				"address_region must name a region of the country, one "+
					"of: GR-14 (Αττική)")

			res = callTool(t, session, "update_checkout", map[string]any{
				"meta": meta(""),
				"id":   created["id"],
				"checkout": map[string]any{
					"line_items": cartLines, "buyer": buyer,
					"fulfillment": fulfillment,
				},
			})
			require.False(t, res.IsError, "%v", res.Content)
			assert.Equal(t, "ready_for_complete",
				structured(t, res)["status"])
		})

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
				"cart_id":  stack.faults.carts.mint(),
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
					"line_items":  cartLines,
					"buyer":       buyer,
					"fulfillment": fulfillment,
					// Selecting the advertised instrument is how an agent
					// says "cash on delivery" — no store-specific id.
					"payment": codInstrument,
				},
			})
			require.False(t, res.IsError, "%v", res.Content)
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
			// The order is attributed to the surface owning the session.
			assert.Equal(t, "ucp", stack.faults.agentProtocol.Load())

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
			"meta":    meta(""),
			"cart_id": stack.faults.carts.mint(),
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
			"checkout": map[string]any{"payment": map[string]any{}},
		})
		require.False(t, res.IsError)
		escalated := structured(t, res)
		assert.Equal(t, "requires_escalation", escalated["status"])
		assert.Equal(t, vivaCheckoutURL, escalated["continue_url"])

		// Django's outbox sends the payment-completed event, and the
		// gateway delivers the signed webhook within the call.
		paid := orderEventAt(time.Now(), "PROCESSING", "COMPLETED")
		require.Equal(t, http.StatusNoContent, stack.sendEvent(t, paid))
		require.Len(t, sink.received, 1, "delivered before the answer")
		<-sink.received
		sink.mu.Lock()
		hook := sink.hooks[0]
		sink.mu.Unlock()

		verifyWebhook(t, hook, key.JWK())
		// Standard Webhooks headers: the event's own id and time.
		assert.Equal(t, paid["eventId"], hook.header.Get("Webhook-Id"))
		assert.Equal(t, eventUnix(t, paid),
			hook.header.Get("Webhook-Timestamp"))
		// The business is named as signer.
		assert.Contains(t, hook.header.Get("UCP-Agent"),
			`profile="https://`)
		assert.Contains(t, hook.header.Get("UCP-Agent"), "/.well-known/ucp")

		// The body is the full order entity.
		var delivered map[string]any
		require.NoError(t, json.Unmarshal(hook.body, &delivered))
		assert.Equal(t, fixtureOrderUUID, delivered["id"])
		assert.Equal(t, checkoutID, delivered["checkout_id"])
		lines := delivered["line_items"].([]any)
		require.Len(t, lines, 1)
		assert.Equal(t, fixtureSoldName,
			lines[0].(map[string]any)["item"].(map[string]any)["title"])
		for _, internal := range []string{"schema", "target_url", "targetUrl"} {
			assert.NotContains(t, delivered, internal)
		}

		// The session is now terminal; complete re-renders the outcome.
		res = callTool(t, session, "complete_checkout", map[string]any{
			"meta":     meta("22222222-2222-2222-2222-222222222222"),
			"id":       checkoutID,
			"checkout": map[string]any{"payment": map[string]any{}},
		})
		require.False(t, res.IsError)
		assert.Equal(t, "completed", structured(t, res)["status"])

		// The session expires within a day; the store reports shipment
		// for weeks after. Events route through the order link, so the
		// platform still hears about them.
		stack.lapse(t, sessionRedisKey(checkoutID))
		shipped := orderEventAt(time.Now(), "SHIPPED", "COMPLETED")
		require.Equal(t, http.StatusNoContent, stack.sendEvent(t, shipped))
		require.Len(t, sink.received, 1,
			"an event after session expiry is delivered")
		<-sink.received
		sink.mu.Lock()
		late := sink.hooks[len(sink.hooks)-1]
		sink.mu.Unlock()
		verifyWebhook(t, late, key.JWK())
		assert.Equal(t, shipped["eventId"], late.header.Get("Webhook-Id"),
			"each event is its own delivery")

		// The outbox settles an event by the platform's answer: one worth
		// another attempt is a 503 carrying the wait the platform asked
		// for, and the attempt after it carries the same Webhook-Id and
		// Webhook-Timestamp; a refusal fails the event for good.
		retried := orderEventAt(time.Now().Add(-time.Hour), "SHIPPED",
			"COMPLETED")
		sink.status.Store(http.StatusServiceUnavailable)
		resp := stack.postEvent(t, retried)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		sink.status.Store(http.StatusOK)
		require.Equal(t, http.StatusNoContent, stack.sendEvent(t, retried))
		sink.status.Store(http.StatusGone)
		assert.Equal(t, http.StatusUnprocessableEntity,
			stack.sendEvent(t, orderEventAt(time.Now(), "SHIPPED",
				"COMPLETED")))
		sink.status.Store(0)
		require.Len(t, sink.received, 3)
		for range 3 {
			<-sink.received
		}
		sink.mu.Lock()
		first, again := sink.hooks[len(sink.hooks)-3], sink.hooks[len(sink.hooks)-2]
		sink.mu.Unlock()
		for _, h := range []string{"Webhook-Id", "Webhook-Timestamp"} {
			assert.Equal(t, first.header.Get(h), again.header.Get(h), h)
		}
		assert.Equal(t, eventUnix(t, retried),
			again.header.Get("Webhook-Timestamp"),
			"the event's time, an hour before the attempt")
	})

	// Complete is a command, not an update: it never places an order at a
	// total the checkout did not show. A payment that selects another
	// method than the checkout holds places nothing and answers incomplete
	// with a recoverable error; the checkout stays as it was, and the key
	// is not spent.
	unready := func(t *testing.T, payment map[string]any) map[string]any {
		t.Helper()
		submit := map[string]any{"buyer": buyer, "fulfillment": fulfillment}
		if payment != nil {
			submit["payment"] = payment
		}
		res := callTool(t, session, "create_checkout", map[string]any{
			"meta":     meta(""),
			"cart_id":  stack.faults.carts.mint(),
			"checkout": submit,
		})
		require.False(t, res.IsError, "%v", res.Content)
		return structured(t, res)
	}
	complete := func(
		t *testing.T, id any, key string, submit map[string]any,
	) *mcp.CallToolResult {
		t.Helper()
		return callTool(t, session, "complete_checkout", map[string]any{
			"meta": meta(key), "id": id, "checkout": submit,
		})
	}
	// placedNothing checks the answer of a complete that changed nothing:
	// incomplete, the recoverable error with code, no order, and the
	// checkout as created.
	placedNothing := func(
		t *testing.T, res *mcp.CallToolResult, created map[string]any,
		code string, orders int32,
	) string {
		t.Helper()
		require.False(t, res.IsError, "%v", res.Content)
		out := structured(t, res)
		assert.Equal(t, "incomplete", out["status"])
		assert.NotContains(t, out, "order")
		msg := out["messages"].([]any)[0].(map[string]any)
		assert.Equal(t, "error", msg["type"])
		assert.Equal(t, code, msg["code"])
		assert.Equal(t, "recoverable", msg["severity"])
		assert.Contains(t, msg["content"], "no order was placed")
		assert.Equal(t, orders, stack.faults.orderCreates.Load())

		res = callTool(t, session, "get_checkout", map[string]any{
			"meta": meta(""), "id": created["id"],
		})
		require.False(t, res.IsError)
		stored := structured(t, res)
		assert.Equal(t, created["status"], stored["status"])
		assert.Equal(t, created["totals"], stored["totals"])
		return msg["content"].(string)
	}

	t.Run("a complete that names another payment method places nothing",
		func(t *testing.T) {
			created := unready(t, codInstrument)
			require.Equal(t, "ready_for_complete", created["status"])
			id := created["id"].(string)
			orders := stack.faults.orderCreates.Load()
			key := uuid.NewString()

			placedNothing(t, complete(t, id, key, map[string]any{
				"payment": map[string]any{}, "pay_way_id": 2,
			}), created, "payment_method_changed", orders)
			assert.EqualValues(t, 1, stack.storedSession(t, id).PayWayID)

			// The checkout's own method, under the same key.
			res := complete(t, id, key, map[string]any{"payment": codInstrument})
			require.False(t, res.IsError, "%v", res.Content)
			assert.Equal(t, "completed", structured(t, res)["status"])
		})

	// The spec requires `payment` on a complete, so a platform may first
	// name its method there. One that leaves the total as the checkout
	// showed it (here the hosted card, free for this cart) is taken, and
	// the order is placed.
	t.Run("an online method named only at complete time places the order",
		func(t *testing.T) {
			created := unready(t, nil)
			require.Equal(t, "incomplete", created["status"])

			res := complete(t, created["id"], uuid.NewString(), map[string]any{
				"payment": map[string]any{}, "pay_way_id": 2,
			})
			require.False(t, res.IsError, "%v", res.Content)
			escalated := structured(t, res)
			assert.Equal(t, "requires_escalation", escalated["status"])
			assert.Equal(t, vivaCheckoutURL, escalated["continue_url"])
			assert.EqualValues(t, 2,
				stack.storedSession(t, created["id"].(string)).PayWayID)
			assert.EqualValues(t, 2, (*stack.faults.lastOrder.Load())["payWayId"])
		})

	// The cart digest holds no pay way, so a checkout that names its
	// first method only at completion is placed from the cart it showed:
	// a cart changed since the platform read it, at the same total, is
	// refused before the completion's quotes or after them, and nothing
	// is placed (#142).
	for name, fault := range map[string]*atomic.Bool{
		"since the platform read it": &stack.faults.lineSwapped,
		"under the completion":       &stack.faults.swapOnReserve,
	} {
		t.Run("a cart changed "+name+" places nothing on a complete "+
			"that names the first method", func(t *testing.T) {
			created := unready(t, nil)
			require.Equal(t, "incomplete", created["status"])
			placed := stack.faults.ordersPlaced.Load()

			fault.Store(true)
			t.Cleanup(func() {
				stack.faults.swapOnReserve.Store(false)
				stack.faults.lineSwapped.Store(false)
			})
			res := complete(t, created["id"], uuid.NewString(),
				map[string]any{"payment": codInstrument})
			require.True(t, res.IsError, "refused: the cart changed")
			assert.Contains(t, res.Content[0].(*mcp.TextContent).Text,
				"cart_changed")
			assert.Equal(t, placed, stack.faults.ordersPlaced.Load())
			assert.EqualValues(t, 1,
				stack.storedSession(t, created["id"].(string)).PayWayID,
				"the method it named is kept")
		})
	}

	// A first method the store does not have is the caller's mistake, not
	// an outage to retry: refused, nothing stored, nothing placed.
	t.Run("an unknown method named only at complete time is refused",
		func(t *testing.T) {
			created := unready(t, nil)
			id := created["id"].(string)
			orders := stack.faults.orderCreates.Load()

			res := complete(t, id, uuid.NewString(), map[string]any{
				"payment": map[string]any{}, "pay_way_id": 99999,
			})
			require.True(t, res.IsError)
			said := res.Content[0].(*mcp.TextContent).Text
			assert.Contains(t, said, `"code":"not_found"`)
			assert.Contains(t, said, "names no payment method")
			assert.Zero(t, stack.storedSession(t, id).PayWayID)
			assert.Equal(t, orders, stack.faults.orderCreates.Load())
		})

	// The method a complete names first is the buyer's choice whether or
	// not that call places the order: a refused one keeps it, and the next
	// complete, which names none, places the order with it.
	t.Run("a refused complete keeps the method it named first",
		func(t *testing.T) {
			created := unready(t, nil)
			id := created["id"].(string)
			orders := stack.faults.orderCreates.Load()

			stack.faults.stockShort.Store(true)
			t.Cleanup(func() { stack.faults.stockShort.Store(false) })
			res := complete(t, id, uuid.NewString(),
				map[string]any{"payment": codInstrument})
			require.True(t, res.IsError, "refused: the stock ran out")
			stack.faults.stockShort.Store(false)
			stored := stack.storedSession(t, id)
			assert.EqualValues(t, 1, stored.PayWayID)
			assert.Equal(t, checkout.StatusReadyForComplete, stored.Status)
			require.Equal(t, orders, stack.faults.orderCreates.Load())

			res = complete(t, id, uuid.NewString(),
				map[string]any{"payment": map[string]any{}})
			require.False(t, res.IsError, "%v", res.Content)
			assert.Equal(t, "completed", structured(t, res)["status"])
			assert.EqualValues(t, 1, (*stack.faults.lastOrder.Load())["payWayId"])
		})

	// A method named first that adds a fee would place the order at a
	// total the checkout never showed. It places nothing, stores nothing,
	// and says the fee and the new total; the platform selects the method
	// with update_checkout, which shows them, and then completes.
	// A method is resolved for the delivery the checkout has then. An
	// update that sets a delivery the method is not offered for, and
	// names none, drops it: the checkout asks for a method, rather than
	// reading ready and failing the order at completion.
	t.Run("an update to a delivery the held method is not offered for drops it",
		func(t *testing.T) {
			res := callTool(t, session, "create_checkout", map[string]any{
				"meta":    meta(""),
				"cart_id": stack.faults.carts.mint(),
				"checkout": map[string]any{
					"buyer": buyer, "payment": codInstrument,
				},
			})
			require.False(t, res.IsError, "%v", res.Content)
			id := structured(t, res)["id"].(string)
			require.EqualValues(t, 1, stack.storedSession(t, id).PayWayID)

			stack.faults.codNotDelivered.Store(true)
			t.Cleanup(func() { stack.faults.codNotDelivered.Store(false) })
			res = callTool(t, session, "update_checkout", map[string]any{
				"meta": meta(""), "id": id,
				"checkout": map[string]any{
					"line_items": cartLines, "buyer": buyer,
					"fulfillment": fulfillment,
				},
			})
			require.False(t, res.IsError, "%v", res.Content)
			assert.Equal(t, "incomplete", structured(t, res)["status"])
			assert.Zero(t, stack.storedSession(t, id).PayWayID)
		})

	// A first method named for a checkout whose delivery the store refuses
	// (a carrier it does not ship by) is not taken, and the answer names
	// the refused delivery, never "no total shown".
	t.Run("a method named first for a refused delivery names the delivery",
		func(t *testing.T) {
			heavy := homeDelivery()
			heavy["methods"].([]any)[0].(map[string]any)["groups"] = []any{
				map[string]any{"id": "parcel",
					"selected_option_id": "carrier_pigeon"}}
			res := callTool(t, session, "create_checkout", map[string]any{
				"meta": meta(""), "cart_id": stack.faults.carts.mint(),
				"checkout": map[string]any{
					"buyer": buyer, "fulfillment": heavy,
				},
			})
			require.False(t, res.IsError, "%v", res.Content)
			id := structured(t, res)["id"].(string)
			orders := stack.faults.orderCreates.Load()

			res = complete(t, id, uuid.NewString(),
				map[string]any{"payment": codInstrument})
			require.True(t, res.IsError, "%v", res.Content)
			text := res.Content[0].(*mcp.TextContent).Text
			assert.Contains(t, text, "refuses one of this checkout's choices")
			assert.Contains(t, text, django.CodeShippingUnavailable)
			assert.Equal(t, orders, stack.faults.orderCreates.Load())
			assert.Zero(t, stack.storedSession(t, id).PayWayID,
				"nothing is stored")
		})

	t.Run("a method named only at complete time that adds a fee places nothing",
		func(t *testing.T) {
			stack.faults.codFee.Store(true)
			t.Cleanup(func() { stack.faults.codFee.Store(false) })
			created := unready(t, nil)
			require.Equal(t, "incomplete", created["status"])
			id := created["id"].(string)
			orders := stack.faults.orderCreates.Load()
			key := uuid.NewString()

			said := placedNothing(t, complete(t, id, key,
				map[string]any{"payment": codInstrument}),
				created, "payment_fee_added", orders)
			assert.Contains(t, said, "a fee of 4.50 EUR")
			assert.Contains(t, said, "the total 933.86 EUR")
			assert.Zero(t, stack.storedSession(t, id).PayWayID)

			res := callTool(t, session, "update_checkout", map[string]any{
				"meta": meta(""), "id": id,
				"checkout": map[string]any{
					"line_items": cartLines, "buyer": buyer,
					"fulfillment": fulfillment, "payment": codInstrument,
				},
			})
			require.False(t, res.IsError, "%v", res.Content)
			updated := structured(t, res)
			require.Equal(t, "ready_for_complete", updated["status"])
			assert.Contains(t, updated["totals"], map[string]any{
				"type": "fee", "display_text": "Payment method fee",
				"amount": float64(450),
			})
			assert.Contains(t, updated["totals"], map[string]any{
				"type": "total", "amount": float64(93386),
			})

			// The same key: the refused complete claimed nothing.
			res = complete(t, id, key, map[string]any{"payment": codInstrument})
			require.False(t, res.IsError, "%v", res.Content)
			assert.Equal(t, "completed", structured(t, res)["status"])
		})

	t.Run("a failed payment link keeps the order and retries the link",
		func(t *testing.T) {
			stack.faults.paymentLink.Store(true)
			res := callTool(t, session, "create_checkout", map[string]any{
				"meta":    meta(""),
				"cart_id": stack.faults.carts.mint(),
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
				"checkout": map[string]any{"payment": map[string]any{}},
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

			stack.faults.paymentLink.Store(false)
			// A read stores what it read from the order while the link is
			// being minted: the order's decision has moved a second on.
			var read atomic.Bool
			var readAnswer atomic.Pointer[mcp.CallToolResult]
			var readStored atomic.Pointer[string]
			before := stack.storedSession(t, checkoutID).Revision
			readMeanwhile := func(r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/create_checkout_session") ||
					!read.CompareAndSwap(false, true) {
					return
				}
				time.Sleep(1100 * time.Millisecond)
				res, err := session.CallTool(t.Context(),
					&mcp.CallToolParams{Name: "get_checkout",
						Arguments: map[string]any{
							"meta": meta(""), "id": checkoutID,
						}})
				if err == nil {
					readAnswer.Store(res)
				}
				// What Redis holds once the read answered, before the
				// complete's own save.
				raw, err := stack.rdb.Get(t.Context(),
					sessionRedisKey(checkoutID)).Result()
				if err == nil {
					readStored.Store(&raw)
				}
			}
			stack.faults.before.Store(&readMeanwhile)
			t.Cleanup(func() { stack.faults.before.Store(nil) })
			res = callTool(t, session, "complete_checkout", map[string]any{
				"meta":     meta(uuid.NewString()),
				"id":       checkoutID,
				"checkout": map[string]any{"payment": map[string]any{}},
			})
			require.True(t, read.Load(), "the read ran meanwhile")
			meanwhile := readAnswer.Load()
			require.NotNil(t, meanwhile, "the read was answered")
			require.False(t, meanwhile.IsError, "%v", meanwhile.Content)
			stored := readStored.Load()
			require.NotNil(t, stored)
			require.NotContains(t, *stored, before,
				"the read stored the checkout before the complete's save")
			require.False(t, res.IsError, "%v", res.Content)
			assert.Equal(t, vivaCheckoutURL,
				structured(t, res)["continue_url"])
			// The link is stored: the next read gives it.
			res = callTool(t, session, "get_checkout", map[string]any{
				"meta": meta(""), "id": checkoutID,
			})
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

	// The order lands and every answer to it is lost: the checkout says
	// the store has not confirmed it. A later complete, under a new key,
	// asks the store again with the checkout's id, which answers the
	// order it placed: the checkout then awaits that order's payment,
	// and no second order is placed.
	t.Run("an unconfirmed order is the one a later complete finds",
		func(t *testing.T) {
			res := callTool(t, session, "create_checkout", map[string]any{
				"meta":    meta(""),
				"cart_id": stack.faults.carts.mint(),
				"checkout": map[string]any{
					"buyer": buyer, "fulfillment": fulfillment,
					"pay_way_id": 2,
				},
			})
			require.False(t, res.IsError)
			checkoutID := structured(t, res)["id"].(string)
			creates := stack.faults.orderCreates.Load()
			placed := stack.faults.ordersPlaced.Load()
			stack.faults.orderAnswerLost.Store(1)
			lost := func(*http.Request) { stack.faults.orderCreate.Store(true) }
			stack.faults.orderHold.Store(&lost)
			t.Cleanup(func() {
				stack.faults.orderHold.Store(nil)
				stack.faults.orderCreate.Store(false)
			})

			res = callTool(t, session, "complete_checkout", map[string]any{
				"meta":     meta(uuid.NewString()),
				"id":       checkoutID,
				"checkout": map[string]any{"payment": map[string]any{}},
			})
			require.True(t, res.IsError)
			assert.Contains(t, res.Content[0].(*mcp.TextContent).Text,
				"has not confirmed whether this checkout's order was placed")
			assert.EqualValues(t, testOrderRetry.Attempts,
				stack.faults.orderCreates.Load()-creates)

			stack.faults.orderHold.Store(nil)
			stack.faults.orderCreate.Store(false)
			res = callTool(t, session, "complete_checkout", map[string]any{
				"meta":     meta(uuid.NewString()),
				"id":       checkoutID,
				"checkout": map[string]any{"payment": map[string]any{}},
			})
			require.False(t, res.IsError, "%v", res.Content)
			out := structured(t, res)
			assert.Equal(t, "requires_escalation", out["status"])
			assert.Equal(t, vivaCheckoutURL, out["continue_url"])
			assert.EqualValues(t, 1, stack.faults.ordersPlaced.Load()-placed,
				"one order for the checkout")
			assert.EqualValues(t, testOrderRetry.Attempts+1,
				stack.faults.orderCreates.Load()-creates)
		})

	t.Run("an unreachable profile is a -32001 discovery error",
		func(t *testing.T) {
			dead := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(dead.Close)
			_, err := session.CallTool(t.Context(),
				&mcp.CallToolParams{Name: "get_checkout", Arguments: map[string]any{
					"meta": map[string]any{"ucp-agent": map[string]any{
						"profile": dead.URL + "/profile.json"}},
					"id": uuid.NewString(),
				}})
			rpcErr, ok := errors.AsType[*jsonrpc.Error](err)
			require.True(t, ok, "want a JSON-RPC error, got %v", err)
			assert.EqualValues(t, -32001, rpcErr.Code)
			var data map[string]string
			require.NoError(t, json.Unmarshal(rpcErr.Data, &data))
			assert.Equal(t, "profile_unreachable", data["code"])
			assert.Contains(t, data["continue_url"], "/cart")
		})

	t.Run("an unsupported protocol version is refused", func(t *testing.T) {
		old := servePlatformProfile(t, platform.URL+"/ucp/orders",
			func(s string) string {
				return strings.Replace(s, `"version": "2026-08-25"`,
					`"version": "2026-01-23"`, 1)
			})
		_, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "get_checkout", Arguments: map[string]any{
				"meta": map[string]any{"ucp-agent": map[string]any{
					"profile": old}},
				"id": uuid.NewString(),
			}})
		rpcErr, ok := errors.AsType[*jsonrpc.Error](err)
		require.True(t, ok, "want a JSON-RPC error, got %v", err)
		var data map[string]string
		require.NoError(t, json.Unmarshal(rpcErr.Data, &data))
		assert.Equal(t, "version_unsupported", data["code"])
	})

	t.Run("no shared checkout version is capabilities_incompatible",
		func(t *testing.T) {
			// Same protocol version, but the platform's checkout entry is
			// an older capability version this business does not offer.
			noCheckout := servePlatformProfile(t, platform.URL+"/ucp/orders",
				func(s string) string {
					return strings.Replace(s,
						`"dev.ucp.shopping.checkout": [
        {
          "version": "2026-08-25"`,
						`"dev.ucp.shopping.checkout": [
        {
          "version": "2026-01-23"`, 1)
				})
			res := callTool(t, session, "create_checkout", map[string]any{
				"meta": map[string]any{"ucp-agent": map[string]any{
					"profile": noCheckout}},
				"cart_id":  stack.faults.carts.mint(),
				"checkout": map[string]any{},
			})
			require.False(t, res.IsError, "a negotiation outcome, not a failure")
			out := structured(t, res)
			assert.Equal(t, "error", out["ucp"].(map[string]any)["status"])
			msg := out["messages"].([]any)[0].(map[string]any)
			assert.Equal(t, "capabilities_incompatible", msg["code"])
			assert.Contains(t, out["continue_url"], "/cart")
		})

	t.Run("responses declare the negotiated capabilities", func(t *testing.T) {
		res := callTool(t, session, "create_checkout", map[string]any{
			"meta": meta(""), "cart_id": stack.faults.carts.mint(),
			"checkout": map[string]any{},
		})
		require.False(t, res.IsError)
		caps := structured(t, res)["ucp"].(map[string]any)["capabilities"].(map[string]any)
		assert.Contains(t, caps, "dev.ucp.shopping.checkout")
		assert.Contains(t, caps, ucp.HostedSelectionCapability)
		assert.NotContains(t, caps, "dev.ucp.shopping.order",
			"order belongs to other operations")
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

	// Django sends an order's first event as the order commits, which can
	// be before the completion that placed it wrote the order's link: an
	// event without one is asked again while that completion may still
	// run, and is settled once it cannot be.
	t.Run("an order event without a link waits for the completion's "+
		"budget", func(t *testing.T) {
		const unknown = "00000000-0000-0000-0000-000000000000"
		fresh := orderEventAt(time.Now(), "PENDING", "PENDING")
		fresh["orderUuid"] = unknown
		resp := stack.postEvent(t, fresh)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		wait, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		require.NoError(t, err)
		budget := checkout.CompletionBudget(ucpUpstreamTimeout)
		assert.Positive(t, wait)
		assert.LessOrEqual(t, time.Duration(wait)*time.Second,
			budget+time.Second)

		old := orderEventAt(time.Now().Add(-budget-time.Second),
			"PENDING", "PENDING")
		old["orderUuid"] = unknown
		assert.Equal(t, http.StatusNoContent, stack.sendEvent(t, old))
	})

	t.Run("an id no checkout can have writes no key", func(t *testing.T) {
		const bogus = "not-a-checkout-id"
		for _, call := range []struct {
			tool string
			args map[string]any
		}{
			{"update_checkout", map[string]any{
				"meta": meta(""), "id": bogus,
				"checkout": map[string]any{
					"line_items": cartLines, "buyer": buyer,
				},
			}},
			{"complete_checkout", map[string]any{
				"meta": meta(uuid.NewString()), "id": bogus,
				"checkout": map[string]any{"payment": codInstrument},
			}},
			{"cancel_checkout", map[string]any{
				"meta": meta(uuid.NewString()), "id": bogus,
			}},
		} {
			assert.Contains(t,
				checkoutGone(t, callTool(t, session, call.tool, call.args)),
				"no longer exists here", call.tool)
		}
		keys, err := stack.rdb.Keys(t.Context(),
			"*"+bogus+"*").Result()
		require.NoError(t, err)
		assert.Empty(t, keys)
	})
}
