//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// flowStack is the checkout flow alone, over a fake Django and real
// Redis: what a completion leaves behind, without a protocol in front.
type flowStack struct {
	flow   *checkout.Flow
	store  *checkout.Store
	rdb    *redis.Client
	faults *checkoutFaults
	tenant *tenant.Tenant
}

func startFlow(t *testing.T, upstreamTimeout time.Duration) flowStack {
	t.Helper()
	faults := new(checkoutFaults)
	djangoSrv := httptest.NewServer(fakeCheckoutDjango(t, faults))
	t.Cleanup(djangoSrv.Close)
	rdb := startRedis(t)
	dj := django.New(djangoSrv.URL+"/api/v1", "api.example.test",
		internalSecret, upstreamTimeout, quietLogger(), nil)
	store := checkout.NewStore(rdb)
	return flowStack{
		flow: checkout.NewFlow(dj, store, storefront.NewSite("el"),
			upstreamTimeout, quietLogger()),
		store: store, rdb: rdb, faults: faults,
		tenant: &tenant.Tenant{
			TenantConfig: django.TenantConfig{
				SchemaName: "demostore", DefaultLocale: "el",
			},
			Domain: "shop.test",
		},
	}
}

// readySession is a cash-on-delivery checkout the fake Django accepts,
// stored as the handlers leave it before they call Complete.
func (f flowStack) readySession(t *testing.T) *checkout.Session {
	t.Helper()
	s := checkout.NewSession(f.tenant.SchemaName, f.tenant.Domain,
		checkout.ProtocolUCP, fixtureCartID)
	s.Buyer = checkout.Buyer{
		FirstName: "Μαρία", LastName: "Παπαδοπούλου",
		Email: "maria@example.test", Phone: "+306912345678",
	}
	s.Fulfillment = checkout.Fulfillment{
		Kind:         checkout.FulfillmentHomeDelivery,
		ProviderCode: django.ShippingProviderACS,
		CountryCode:  "GR", City: "Αθήνα", Zipcode: "10431",
		Street: "Πανεπιστημίου", StreetNumber: "12", RegionID: "GR-14",
	}
	s.PayWayID = 1
	s.Recompute()
	require.Equal(t, checkout.StatusReadyForComplete, s.Status)
	require.NoError(t, f.store.Save(context.Background(), s))
	return s
}

func (f flowStack) stored(t *testing.T, s *checkout.Session) *checkout.Session {
	t.Helper()
	got, err := f.store.Load(context.Background(), s.Schema, s.ID)
	require.NoError(t, err)
	return got
}

// redisStep runs before each Redis command, at exactly that step of the
// flow. An error it returns is the command's result, and the command is
// not sent.
type redisStep func(redis.Cmder) error

func (redisStep) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (step redisStep) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := step(cmd); err != nil {
			return err
		}
		return next(ctx, cmd)
	}
}

func (redisStep) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return next
}

// setsKey reports whether cmd writes a key that contains part.
func setsKey(cmd redis.Cmder, part string) bool {
	args := cmd.Args()
	return cmd.Name() == "set" && len(args) > 1 &&
		strings.Contains(fmt.Sprint(args[1]), part)
}

// The order link is the first thing written once Django has answered. A
// caller that leaves right then must still find its order linked and its
// checkout saved as placed: Django has the order whatever the caller did.
func TestCompleteSavesAPlacedOrderAfterTheCallerLeft(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if setsKey(cmd, ":orderlink:") {
			cancel()
		}
		return nil
	}))

	placement, err := f.flow.Complete(ctx, f.tenant, s)

	require.Error(t, ctx.Err(), "the caller left during the completion")
	require.NoError(t, err)
	assert.Equal(t, checkout.OrderMayExist, placement)
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusCompleted, stored.Status)
	assert.Equal(t, fixtureOrderUUID, stored.OrderUUID)
	link, err := f.store.OrderLinkFor(
		context.Background(), s.Schema, fixtureOrderUUID)
	require.NoError(t, err, "the order link exists")
	assert.Equal(t, s.ID, link.CheckoutID)
}

// A caller that gives up while Django is still creating the order does
// not end the creation: Django answers, and the checkout is saved as
// placed.
func TestCompleteFinishesAnOrderAnsweredAfterTheCallerLeft(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold := func(r *http.Request) {
		cancel()
		// Long enough for a creation still tied to the caller to be
		// dropped, which ends this request.
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
	f.faults.orderHold.Store(&hold)

	placement, err := f.flow.Complete(ctx, f.tenant, s)

	require.NoError(t, err)
	assert.Equal(t, checkout.OrderMayExist, placement)
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusCompleted, stored.Status)
	assert.Equal(t, fixtureOrderUUID, stored.OrderUUID)
	link, err := f.store.OrderLinkFor(
		context.Background(), s.Schema, fixtureOrderUUID)
	require.NoError(t, err, "the order link exists")
	assert.Equal(t, s.ID, link.CheckoutID)
}

// Only Django's silence makes the outcome unknown, never the caller
// leaving: the creation waits out the upstream timeout, and the checkout
// then stays in progress so that nothing can place it a second time.
func TestCompleteLeavesAnUnansweredOrderInProgress(t *testing.T) {
	const upstreamTimeout = time.Second
	f := startFlow(t, upstreamTimeout)
	s := f.readySession(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold := func(r *http.Request) {
		cancel()
		<-r.Context().Done()
	}
	f.faults.orderHold.Store(&hold)

	started := time.Now()
	placement, err := f.flow.Complete(ctx, f.tenant, s)

	require.ErrorIs(t, err, checkout.ErrOrderOutcomeUnknown)
	assert.Equal(t, checkout.OrderMayExist, placement,
		"the key is spent: the order may exist")
	assert.NotErrorIs(t, err, context.Canceled,
		"ended by the store's silence, not by the caller leaving")
	assert.GreaterOrEqual(t, time.Since(started), upstreamTimeout)
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusCompleteInProgress, stored.Status)
	assert.Empty(t, stored.OrderUUID)
}

// A refusal created nothing, so the checkout is stored ready again: left
// in progress it would answer "in flight" until it expired.
func TestCompleteRefusalStoresTheSessionReadyAgain(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	f.faults.orderRefuse.Store(true)

	placement, err := f.flow.Complete(context.Background(), f.tenant, s)

	require.ErrorIs(t, err, django.ErrValidation)
	assert.Equal(t, checkout.NothingPlaced, placement,
		"the key is released: nothing was created")
	assert.Equal(t, checkout.StatusReadyForComplete, s.Status)
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusReadyForComplete, stored.Status)
	assert.Empty(t, stored.OrderUUID)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load())
}

// A session that is not admitted is refused before anything is created,
// and never written: a snapshot that reads in progress belongs to another
// attempt, and saving it would overwrite that attempt's session.
func TestCompleteStoresNothingForASessionItDoesNotAdmit(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	cases := []struct {
		name string
		edit func(*checkout.Session)
		want error
	}{
		{"another attempt's snapshot", func(s *checkout.Session) {
			s.Status = checkout.StatusCompleteInProgress
		}, checkout.ErrCompletionInProgress},
		{"a pay way the store does not offer", func(s *checkout.Session) {
			s.PayWayID = 99
		}, checkout.ErrPayWayUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := f.readySession(t)
			tc.edit(s)
			s.Buyer.FirstName = "unsaved"

			placement, err := f.flow.Complete(
				context.Background(), f.tenant, s)

			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, checkout.NothingPlaced, placement)
			stored := f.stored(t, s)
			assert.Equal(t, checkout.StatusReadyForComplete, stored.Status)
			assert.Equal(t, "Μαρία", stored.Buyer.FirstName)
			assert.Zero(t, f.faults.orderCreates.Load())
		})
	}
}

// The in-progress save is what guards the order. When Redis refuses it
// no order is sent, and the session is not written again: that write
// would be a ready snapshot saved over whatever the store holds by then.
func TestCompleteSendsNoOrderWithoutTheInProgressSave(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	redisDown := errors.New("redis is down")
	writes := 0
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() == "get" || len(args) < 2 ||
			!strings.HasSuffix(fmt.Sprint(args[1]), ":cs:"+s.ID) {
			return nil
		}
		writes++
		return redisDown
	}))

	placement, err := f.flow.Complete(context.Background(), f.tenant, s)

	require.ErrorIs(t, err, redisDown)
	assert.Equal(t, checkout.NothingPlaced, placement)
	assert.Equal(t, 1, writes, "refused once, and not written again")
	assert.Zero(t, f.faults.orderCreates.Load())
	assert.Equal(t, checkout.StatusReadyForComplete, s.Status)
	assert.Equal(t, checkout.StatusReadyForComplete, f.stored(t, s).Status)
}

// The order exists once Django has answered, whatever happens to the
// session after: a save that fails is reported as that, with the key
// spent, and the stored session stays in progress, so no retry can find
// a ready session to place again.
func TestCompleteReportsAPlacedOrderItCouldNotSave(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	redisDown := errors.New("redis is down")
	linked := false
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		switch {
		case setsKey(cmd, ":orderlink:"):
			linked = true
		case linked && setsKey(cmd, ":cs:"+s.ID):
			return redisDown
		}
		return nil
	}))

	placement, err := f.flow.Complete(context.Background(), f.tenant, s)

	require.ErrorIs(t, err, checkout.ErrSessionNotSaved)
	assert.Equal(t, checkout.OrderMayExist, placement)
	assert.Equal(t, fixtureOrderUUID, s.OrderUUID)
	assert.Equal(t, checkout.StatusCompleteInProgress, f.stored(t, s).Status)
	link, err := f.store.OrderLinkFor(
		context.Background(), s.Schema, fixtureOrderUUID)
	require.NoError(t, err, "the order link exists")
	assert.Equal(t, s.ID, link.CheckoutID)
}
