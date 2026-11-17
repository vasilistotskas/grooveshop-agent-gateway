//go:build integration

package integration

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// codPayWay is how pay_way.json lists cash on delivery (pay way 1).
const codPayWay = `"providerCode":"cash_on_delivery","settlement":"courier_cash"`

// branchDjango counts the calls the flow's branches must or must not
// make, and serves cash on delivery as payWay says when it is set.
type branchDjango struct {
	payWay       atomic.Pointer[string]
	reserves     atomic.Int32
	paymentLinks atomic.Int32
	orderReads   atomic.Int32
}

func (b *branchDjango) wrap(t *testing.T, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost &&
			r.URL.Path == "/api/v1/cart/reserve-stock":
			b.reserves.Add(1)
		case r.Method == http.MethodPost &&
			strings.HasSuffix(r.URL.Path, "/create_checkout_session"):
			b.paymentLinks.Add(1)
		case r.Method == http.MethodGet &&
			strings.HasPrefix(r.URL.Path, "/api/v1/agent/internal/order/"):
			b.orderReads.Add(1)
		}
		as := b.payWay.Load()
		if as == nil || r.Method != http.MethodGet ||
			r.URL.Path != "/api/v1/pay_way" {
			inner.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		body := rec.Body.String()
		if !assert.Contains(t, body, codPayWay) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write([]byte(strings.Replace(body, codPayWay, *as, 1)))
	})
}

// startBranchFlow is startFlow over a Django b watches, for a store
// with agent commerce on and the hosted-payment gate as hosted says.
func startBranchFlow(t *testing.T, hosted bool) (flowStack, *branchDjango) {
	t.Helper()
	faults := new(checkoutFaults)
	b := new(branchDjango)
	djangoSrv := httptest.NewServer(b.wrap(t, fakeCheckoutDjango(t, faults)))
	t.Cleanup(djangoSrv.Close)
	rdb := startRedis(t)
	dj := django.New(djangoSrv.URL+"/api/v1", "api.example.test",
		internalSecret, 5*time.Second, quietLogger(), nil)
	store := checkout.NewStore(rdb, 5*time.Second)
	return flowStack{
		flow: checkout.NewFlow(dj, store, storefront.NewSite("el"),
			quietLogger()),
		store: store, rdb: rdb, faults: faults,
		tenant: &tenant.Tenant{
			SchemaName: "demostore", DefaultLocale: "el",
			AgentCommerceEnabled:      true,
			AgentHostedPaymentEnabled: hosted,
			Domain:                    "shop.test",
		},
	}, b
}

// storedBytes is the session exactly as Redis holds it.
func (f flowStack) storedBytes(t *testing.T, id string) string {
	t.Helper()
	raw, err := f.rdb.Get(t.Context(), sessionRedisKey(id)).Result()
	require.NoError(t, err)
	return raw
}

// Complete places an order only with a pay way it can finish: one it
// knows how money is taken for, Viva's hosted page while the store's
// gate is on, or a collect-later one. Every other is refused before the
// stock is reserved, and the session is left as it was stored.
func TestCompleteRefusesAPayWayItCannotFinish(t *testing.T) {
	cases := []struct {
		name     string
		payWay   string
		protocol string
		hosted   bool
		detail   string
	}{
		{
			name:     "a settlement it does not know",
			payWay:   `"providerCode":"viva_wallet","settlement":"barter"`,
			protocol: checkout.ProtocolUCP, hosted: true,
			detail: `reports settlement "barter"`,
		},
		{
			name:     "no settlement at all",
			payWay:   `"providerCode":"cash_on_delivery","settlement":""`,
			protocol: checkout.ProtocolUCP, hosted: true,
			detail: `reports settlement ""`,
		},
		{
			name:     "an online pay way that is not Viva",
			payWay:   `"providerCode":"stripe","settlement":"online"`,
			protocol: checkout.ProtocolUCP, hosted: true,
		},
		{
			name:     "Viva with the hosted gate switched off since",
			payWay:   `"providerCode":"viva_wallet","settlement":"online"`,
			protocol: checkout.ProtocolUCP, hosted: false,
			detail: "hosted payment is disabled",
		},
		{
			name:     "an online pay way on a surface that does not choose",
			payWay:   `"providerCode":"viva_wallet","settlement":"online"`,
			protocol: checkout.ProtocolACP, hosted: true,
			detail: "completes only with a collect-later one",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, b := startBranchFlow(t, tc.hosted)
			s := f.readySession(t)
			if tc.protocol != s.Protocol {
				s = f.sessionOf(t, tc.protocol)
			}
			before := f.storedBytes(t, s.ID)
			b.payWay.Store(&tc.payWay)

			placement, err := f.flow.Complete(
				t.Context(), f.tenant, s)

			require.ErrorIs(t, err, checkout.ErrPaymentMethodUnsupported)
			if tc.detail == "" {
				// Refused with the bare sentinel, not one of the
				// refusals above.
				assert.Equal(t,
					checkout.ErrPaymentMethodUnsupported.Error(), err.Error())
			} else {
				assert.Contains(t, err.Error(), tc.detail)
			}
			assert.Equal(t, checkout.NothingPlaced, placement)
			assert.Zero(t, b.reserves.Load(), "stock was reserved")
			assert.Zero(t, f.faults.orderCreates.Load())
			assert.Equal(t, before, f.storedBytes(t, s.ID))
		})
	}

	// The control: Viva with the gate on is placed and opens its page.
	t.Run("Viva with the hosted gate on", func(t *testing.T) {
		f, b := startBranchFlow(t, true)
		s := f.readySession(t)
		viva := `"providerCode":"viva_wallet","settlement":"online"`
		b.payWay.Store(&viva)

		placement, err := f.flow.Complete(t.Context(), f.tenant, s)

		require.NoError(t, err)
		assert.Equal(t, checkout.OrderMayExist, placement)
		stored := f.stored(t, s)
		assert.Equal(t, checkout.StatusRequiresEscalation, stored.Status)
		assert.Equal(t, vivaCheckoutURL, stored.PaymentURL)
	})
}

// sessionOf is readySession on another protocol.
func (f flowStack) sessionOf(t *testing.T, protocol string) *checkout.Session {
	t.Helper()
	ready := f.readySession(t)
	s := checkout.NewSession(f.tenant.SchemaName, f.tenant.Domain, protocol,
		f.faults.carts.mint())
	s.Buyer, s.Fulfillment, s.PayWayID = ready.Buyer, ready.Fulfillment,
		ready.PayWayID
	s.Recompute()
	require.Equal(t, checkout.StatusReadyForComplete, s.Status)
	require.NoError(t, f.store.Create(t.Context(), s,
		checkout.CartHolder{}))
	return s
}

// escalated is a session whose Viva order is placed, as Complete leaves
// it when the payment page could not be opened.
func escalated() *checkout.Session {
	s := checkout.NewSession("demostore", "shop.test", checkout.ProtocolUCP,
		fixtureCartID)
	s.Status = checkout.StatusRequiresEscalation
	s.OrderID, s.OrderUUID = 684, fixtureOrderUUID
	return s
}

// ResumePayment opens a payment page only for an escalated order that
// has none; any other session is left as it is, with no call to Django.
func TestResumePaymentOpensAPageOnlyWhereOneIsMissing(t *testing.T) {
	f, b := startBranchFlow(t, true)
	ctx := t.Context()
	// The order awaits its online payment, its hold ahead.
	f.faults.orderOnline.Store(true)

	t.Run("an escalated order without a page", func(t *testing.T) {
		s := escalated()
		require.NoError(t, f.flow.ResumePayment(ctx, f.tenant, s))
		assert.Equal(t, vivaCheckoutURL, s.PaymentURL)
		assert.Equal(t, checkout.StatusRequiresEscalation, s.Status)
	})
	calls := b.paymentLinks.Load()
	require.EqualValues(t, 1, calls)

	for name, edit := range map[string]func(*checkout.Session){
		"a page already open": func(s *checkout.Session) {
			s.PaymentURL = "https://pay.example.test/kept"
		},
		"no order": func(s *checkout.Session) {
			s.OrderID, s.OrderUUID = 0, ""
		},
		"not escalated": func(s *checkout.Session) {
			s.Status = checkout.StatusCompleted
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := escalated()
			edit(s)
			want := *s
			require.NoError(t, f.flow.ResumePayment(ctx, f.tenant, s))
			assert.Equal(t, want, *s)
			assert.Equal(t, calls, b.paymentLinks.Load())
		})
	}

	// Once its hold has passed the checkout says the store cancels the
	// order: no page is opened for it, though Django would open one.
	t.Run("a hold that has passed", func(t *testing.T) {
		f.faults.holdPassed.Store(true)
		defer f.faults.holdPassed.Store(false)
		s := escalated()
		err := f.flow.ResumePayment(ctx, f.tenant, s)
		require.ErrorIs(t, err, checkout.ErrPaymentWindowClosed)
		assert.Empty(t, s.PaymentURL)
		assert.Equal(t, calls, b.paymentLinks.Load())
	})

	// No hold is Django's to judge: a canceled payment can be retried.
	t.Run("an order with no hold", func(t *testing.T) {
		f.faults.orderOnline.Store(false)
		defer f.faults.orderOnline.Store(true)
		s := escalated()
		require.NoError(t, f.flow.ResumePayment(ctx, f.tenant, s))
		assert.Equal(t, vivaCheckoutURL, s.PaymentURL)
		calls = b.paymentLinks.Load()
	})

	t.Run("an order that cannot be read", func(t *testing.T) {
		f.faults.orderRead.Store(true)
		defer f.faults.orderRead.Store(false)
		s := escalated()
		err := f.flow.ResumePayment(ctx, f.tenant, s)
		require.ErrorIs(t, err, checkout.ErrPaymentSessionFailed)
		assert.Equal(t, calls, b.paymentLinks.Load())
	})

	t.Run("a page that cannot be opened", func(t *testing.T) {
		f.faults.paymentLink.Store(true)
		defer f.faults.paymentLink.Store(false)
		s := escalated()
		err := f.flow.ResumePayment(ctx, f.tenant, s)
		require.ErrorIs(t, err, checkout.ErrPaymentSessionFailed)
		assert.Empty(t, s.PaymentURL)
		assert.Equal(t, checkout.StatusRequiresEscalation, s.Status)
	})
}

// An order event for a checkout that is no longer stored, or that has
// ended, changes nothing and asks Django nothing.
func TestOrderEventLeavesAGoneOrEndedCheckoutAlone(t *testing.T) {
	f, b := startBranchFlow(t, true)
	ctx := t.Context()

	t.Run("no session", func(t *testing.T) {
		id := "4d3c2b1a-0000-4000-8000-000000000001"
		require.NoError(t, f.flow.ApplyOrderEvent(ctx, "demostore", id,
			fixtureOrderUUID, "PROCESSING", django.PaymentStatusCompleted))
		_, err := f.rdb.Get(ctx, sessionRedisKey(id)).Result()
		require.ErrorIs(t, err, redis.Nil, "the event stored a session")
		assert.Zero(t, b.orderReads.Load())
	})

	for _, status := range []checkout.Status{
		checkout.StatusCanceled, checkout.StatusCompleted,
	} {
		t.Run(string(status), func(t *testing.T) {
			s := f.readySession(t)
			s.Status = status
			require.NoError(t, f.store.Save(ctx, s))
			before := f.storedBytes(t, s.ID)

			require.NoError(t, f.flow.ApplyOrderEvent(ctx, s.Schema, s.ID,
				fixtureOrderUUID, "PROCESSING", django.PaymentStatusCompleted))

			assert.Equal(t, before, f.storedBytes(t, s.ID))
			assert.Zero(t, b.orderReads.Load())
		})
	}
}

// An event that is not a payment, for a checkout that already names its
// order, has nothing to record: the session is not written.
func TestOrderEventWithNothingNewWritesNothing(t *testing.T) {
	f, b := startBranchFlow(t, true)
	ctx := t.Context()
	s := f.readySession(t)
	s.Status = checkout.StatusRequiresEscalation
	s.OrderID, s.OrderUUID = 684, fixtureOrderUUID
	s.PlacedAt = time.Now().UTC()
	require.NoError(t, f.store.Save(ctx, s))
	before := f.storedBytes(t, s.ID)
	writes := 0
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if savesKey(cmd, sessionRedisKey(s.ID)) {
			writes++
		}
		return nil
	}))

	require.NoError(t, f.flow.ApplyOrderEvent(ctx, s.Schema, s.ID,
		fixtureOrderUUID, "PENDING", "PROCESSING"))

	assert.Zero(t, writes)
	assert.Equal(t, before, f.storedBytes(t, s.ID))
	assert.Zero(t, b.orderReads.Load())
}

// An event's save is a compare-and-set like every other: one that lost
// to a write made since it read the session reports it, so the event is
// pushed again, and the other write stands.
func TestOrderEventThatLostItsSaveSaysSo(t *testing.T) {
	f, _ := startBranchFlow(t, true)
	ctx := t.Context()
	s := f.readySession(t)
	s.Status = checkout.StatusRequiresEscalation
	s.OrderID, s.OrderUUID = 684, fixtureOrderUUID
	s.PlacedAt = time.Now().UTC()
	require.NoError(t, f.store.Save(ctx, s))
	var armed atomic.Bool
	armed.Store(true)
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if !savesKey(cmd, sessionRedisKey(s.ID)) ||
			!armed.CompareAndSwap(true, false) {
			return nil
		}
		other, err := f.store.LoadAnyProtocol(ctx, s.Schema, s.ID)
		if err != nil {
			return err
		}
		other.Buyer.FirstName = "Ελένη"
		return f.store.Save(ctx, other)
	}))

	err := f.flow.ApplyOrderEvent(ctx, s.Schema, s.ID,
		fixtureOrderUUID, "PROCESSING", django.PaymentStatusCompleted)

	require.ErrorIs(t, err, checkout.ErrSessionChanged)
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusRequiresEscalation, stored.Status)
	assert.Equal(t, "Ελένη", stored.Buyer.FirstName)
}
