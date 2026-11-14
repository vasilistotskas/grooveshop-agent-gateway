//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
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
	store := checkout.NewStore(rdb, upstreamTimeout)
	return flowStack{
		flow: checkout.NewFlow(dj, store, storefront.NewSite("el"),
			quietLogger()),
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
		checkout.ProtocolUCP, f.faults.carts.mint())
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
	require.NoError(t, f.store.Create(context.Background(), s,
		checkout.CartHolder{}))
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

// setsKey reports whether cmd writes a key that contains part: a SET, or
// a script, which names its key after its hash and its key count.
func setsKey(cmd redis.Cmder, part string) bool {
	at := map[string]int{"set": 1, "evalsha": 3, "eval": 3}[cmd.Name()]
	args := cmd.Args()
	return at > 0 && len(args) > at &&
		strings.Contains(fmt.Sprint(args[at]), part)
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
	// The order was sent and never answered: it may exist, so the
	// session is kept as long as a placed one.
	assert.True(t, stored.Unconfirmed)
	assert.Greater(t, f.lifetime(t, s), 23*time.Hour)
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

// A completion marks the session in progress only over the session it
// loaded. One that another request has stored since (it placed the
// order, changed the buyer, canceled) is not this completion's to place:
// it sends nothing to the store, writes nothing and frees its key.
func TestCompleteLosesToASessionStoredSinceItLoadedIt(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	ctx := context.Background()
	save := func(edit func(*checkout.Session)) func(*testing.T, *checkout.Session) {
		return func(t *testing.T, other *checkout.Session) {
			t.Helper()
			edit(other)
			require.NoError(t, f.store.Save(ctx, other))
		}
	}
	for name, meanwhile := range map[string]func(*testing.T, *checkout.Session){
		"a completion that placed the order": func(
			t *testing.T, other *checkout.Session,
		) {
			t.Helper()
			placement, err := f.flow.Complete(ctx, f.tenant, other)
			require.NoError(t, err)
			require.Equal(t, checkout.OrderMayExist, placement)
		},
		"an update": save(func(other *checkout.Session) {
			other.Buyer.Email = "other@example.test"
		}),
		"a cancel": save(func(other *checkout.Session) {
			other.Status = checkout.StatusCanceled
		}),
	} {
		t.Run(name, func(t *testing.T) {
			s := f.readySession(t)
			meanwhile(t, f.stored(t, s))
			creates := f.faults.orderCreates.Load()
			before, err := f.rdb.Get(ctx, sessionRedisKey(s.ID)).Result()
			require.NoError(t, err)

			placement, err := f.flow.Complete(ctx, f.tenant, s)

			require.ErrorIs(t, err, checkout.ErrSessionChanged)
			assert.Equal(t, checkout.NothingPlaced, placement)
			assert.Equal(t, creates, f.faults.orderCreates.Load(),
				"an order was sent for a session that had moved on")
			after, err := f.rdb.Get(ctx, sessionRedisKey(s.ID)).Result()
			require.NoError(t, err)
			assert.Equal(t, before, after, "the stored session was written")
		})
	}
}

// However many completions run at once on one session, with no lock and
// no key between them, one order is sent: Redis lets one of them mark
// the session in progress, and every other one has placed nothing. They
// all read the same state and save within the same instant, so their
// writes differ in nothing but the revision each gives its own.
func TestCompletesAtOncePlaceOneOrder(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	ctx := context.Background()
	s := f.readySession(t)
	type outcome struct {
		placement checkout.Placement
		err       error
	}
	const racers = 16
	start := make(chan struct{})
	outcomes := make(chan outcome, racers)
	for range racers {
		own := f.stored(t, s)
		go func() {
			<-start
			placement, err := f.flow.Complete(ctx, f.tenant, own)
			outcomes <- outcome{placement, err}
		}()
	}
	close(start)

	placed := 0
	for range racers {
		o := <-outcomes
		if o.err == nil {
			placed++
			assert.Equal(t, checkout.OrderMayExist, o.placement)
			continue
		}
		assert.ErrorIs(t, o.err, checkout.ErrSessionChanged)
		assert.Equal(t, checkout.NothingPlaced, o.placement)
	}
	assert.Equal(t, 1, placed)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load())
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusCompleted, stored.Status)
	assert.Equal(t, fixtureOrderUUID, stored.OrderUUID)
}

// lifetime is how long Redis still keeps the session.
func (f flowStack) lifetime(t *testing.T, s *checkout.Session) time.Duration {
	t.Helper()
	ttl, err := f.rdb.PTTL(context.Background(),
		sessionRedisKey(s.ID)).Result()
	require.NoError(t, err)
	return ttl
}

// Django refused the order: none exists, definitely. If the save that
// says so fails, the stored session still reads in progress. It gets one
// more attempt, and if that fails too the session keeps the half hour of
// an open one: it must not claim for a day that an order may exist.
func TestARefusedOrderDoesNotLeaveASessionClaimingOne(t *testing.T) {
	for name, tc := range map[string]struct {
		failures int
		want     checkout.Status
	}{
		"the save fails once":  {1, checkout.StatusReadyForComplete},
		"the save fails twice": {2, checkout.StatusCompleteInProgress},
	} {
		t.Run(name, func(t *testing.T) {
			f := startFlow(t, 5*time.Second)
			s := f.readySession(t)
			f.faults.orderRefuse.Store(true)
			redisDown := errors.New("redis is down")
			saves := 0
			f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
				if !savesKey(cmd, sessionRedisKey(s.ID)) {
					return nil
				}
				// The first save marks it in progress.
				if saves++; saves > 1 && saves <= 1+tc.failures {
					return redisDown
				}
				return nil
			}))

			placement, err := f.flow.Complete(
				context.Background(), f.tenant, s)

			require.ErrorIs(t, err, django.ErrValidation)
			assert.Equal(t, checkout.NothingPlaced, placement)
			assert.Equal(t, tc.want, f.stored(t, s).Status)
			assert.LessOrEqual(t, f.lifetime(t, s), 30*time.Minute)
			assert.EqualValues(t, 1, f.faults.orderCreates.Load())
		})
	}
}

// The order link is what order events, get_order and the webhooks find a
// checkout's order by. A failed write of the record beside the session
// does not cost it.
func TestAFailedOrderRecordStillLinksTheOrder(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	ctx := context.Background()
	s := f.readySession(t)
	record := sessionRedisKey(s.ID) + ":order"
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if args := cmd.Args(); cmd.Name() == "set" && len(args) > 1 &&
			args[1] == record {
			return errors.New("redis is down")
		}
		return nil
	}))

	placement, err := f.flow.Complete(ctx, f.tenant, s)

	require.NoError(t, err)
	assert.Equal(t, checkout.OrderMayExist, placement)
	link, err := f.store.OrderLinkFor(ctx, s.Schema, fixtureOrderUUID)
	require.NoError(t, err, "the order link exists")
	assert.Equal(t, s.ID, link.CheckoutID)
	assert.Zero(t, f.rdb.Exists(ctx, record).Val())
}

// replyLost lets a command reach Redis and then fails it when lose says
// so: a connection that drops after the write and before its reply.
type replyLost func(redis.Cmder) bool

func (replyLost) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (lose replyLost) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if err == nil && lose(cmd) {
			return errors.New("connection reset after the write")
		}
		return err
	}
}

func (replyLost) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return next
}

// The save after the order call can be stored and lose only its reply.
// Its second attempt sends the same write, so it finds its own: the
// order is placed and its session saved, not a session another writer
// stored. Paid online, the session awaits its payment, which an order
// event never leaves, so only recognising its own write can save it.
func TestAPlacedOrdersSaveWhoseReplyWasLostIsItsOwn(t *testing.T) {
	ctx := context.Background()
	f := startFlow(t, 5*time.Second)
	f.tenant.AgentCommerceEnabled = true
	f.tenant.AgentHostedPaymentEnabled = true
	s := f.readySession(t)
	s.PayWayID = 2
	require.NoError(t, f.store.Save(ctx, s))
	var linked, lost atomic.Bool
	f.rdb.AddHook(replyLost(func(cmd redis.Cmder) bool {
		if setsKey(cmd, ":orderlink:") {
			linked.Store(true)
			return false
		}
		return linked.Load() && savesKey(cmd, sessionRedisKey(s.ID)) &&
			lost.CompareAndSwap(false, true)
	}))

	placement, err := f.flow.Complete(ctx, f.tenant, s)

	require.True(t, lost.Load(), "the first save's reply was lost")
	require.NoError(t, err)
	assert.Equal(t, checkout.OrderMayExist, placement)
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusRequiresEscalation, stored.Status)
	assert.Equal(t, fixtureOrderUUID, stored.OrderUUID)
	assert.Equal(t, s.Revision, stored.Revision)
}

// The mark that guards the order can be stored and lose only its reply.
// Taken for a failure, it would leave a session that reads as being
// placed, then as an order whose outcome was never recorded, while no
// order was sent. The second attempt sends the same write, finds its
// own, and the order is placed once.
func TestAMarkWhoseReplyWasLostPlacesTheOrder(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	var lost atomic.Bool
	f.rdb.AddHook(replyLost(func(cmd redis.Cmder) bool {
		return savesKey(cmd, sessionRedisKey(s.ID)) &&
			lost.CompareAndSwap(false, true)
	}))

	placement, err := f.flow.Complete(context.Background(), f.tenant, s)

	require.True(t, lost.Load(), "the mark's reply was lost")
	require.NoError(t, err)
	assert.Equal(t, checkout.OrderMayExist, placement)
	assert.Equal(t, checkout.StatusCompleted, f.stored(t, s).Status)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load())
}

// An order call that went unanswered leaves an order that may exist.
// The save that says so, and keeps the session a day, gets the same
// second attempt when Redis fails it.
func TestAnUnansweredOrdersMarkIsTriedAgain(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	f.faults.orderCreate.Store(true)
	saves := 0
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if !savesKey(cmd, sessionRedisKey(s.ID)) {
			return nil
		}
		// The first save marks it in progress; the second fails.
		if saves++; saves == 2 {
			return errors.New("redis is down")
		}
		return nil
	}))

	placement, err := f.flow.Complete(context.Background(), f.tenant, s)

	require.ErrorIs(t, err, checkout.ErrOrderOutcomeUnknown)
	assert.Equal(t, checkout.OrderMayExist, placement)
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusCompleteInProgress, stored.Status)
	assert.True(t, stored.Unconfirmed)
	assert.Greater(t, f.lifetime(t, s), 23*time.Hour)
}

// A session in progress can still be stored before its completion's
// last save, by the one other writer of such a session: an order event,
// for an order that is paid at once. The completion stores its outcome
// over the event's, and what the event said (the order is paid) is kept.
func TestCompleteStoresItsOutcomeOverAnOrderEvent(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	ctx := context.Background()
	s := f.readySession(t)
	placed := time.Now()
	f.faults.orderCreatedAt.Store(&placed)
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if !setsKey(cmd, ":orderlink:") {
			return nil
		}
		// The order exists and its link is being written: its payment
		// event lands before the completion's last save.
		return f.flow.ApplyOrderEvent(ctx, s.Schema, s.ID,
			fixtureOrderUUID, django.PaymentStatusCompleted)
	}))

	placement, err := f.flow.Complete(ctx, f.tenant, s)

	require.NoError(t, err)
	assert.Equal(t, checkout.OrderMayExist, placement)
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusCompleted, stored.Status)
	assert.Equal(t, fixtureOrderUUID, stored.OrderUUID)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load())
}

// A completion that placed its order and could not save leaves a session
// in progress that does not name the order. An order event finds it
// through the order link and records the order, read from Django by the
// event's uuid: its id, and its creation time, from which the session's
// day counts. A payment event then completes it.
func TestOrderEventRecordsTheOrderItsSessionNeverSaved(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	ctx := context.Background()
	s := f.readySession(t)
	redisDown := errors.New("redis is down")
	var linked, down atomic.Bool
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		switch {
		case setsKey(cmd, ":orderlink:"):
			linked.Store(true)
			down.Store(true)
		case linked.Load() && down.Load() &&
			setsKey(cmd, sessionRedisKey(s.ID)):
			return redisDown
		}
		return nil
	}))
	_, err := f.flow.Complete(ctx, f.tenant, s)
	require.ErrorIs(t, err, checkout.ErrSessionNotSaved)
	down.Store(false)
	unsaved := f.stored(t, s)
	require.Equal(t, checkout.StatusCompleteInProgress, unsaved.Status)
	require.Empty(t, unsaved.OrderUUID)
	// The record beside the session is not what the event reads.
	require.NoError(t, f.rdb.Del(ctx, sessionRedisKey(s.ID)+":order").Err())

	// The order was placed an hour ago, whenever its event comes.
	placed := time.Now().Add(-time.Hour)
	f.faults.orderCreatedAt.Store(&placed)
	require.NoError(t, f.flow.ApplyOrderEvent(ctx, s.Schema, s.ID,
		fixtureOrderUUID, "PENDING"))

	named := f.stored(t, s)
	assert.Equal(t, checkout.StatusCompleteInProgress, named.Status)
	assert.Equal(t, fixtureOrderUUID, named.OrderUUID)
	assert.EqualValues(t, 684, named.OrderID, "the fixture order's id")
	assert.WithinDuration(t, placed, named.PlacedAt, time.Second)
	// So the session ends a day after the order, not after the event.
	ttl, err := f.rdb.PTTL(ctx, sessionRedisKey(s.ID)).Result()
	require.NoError(t, err)
	assert.InDelta(t, (23 * time.Hour).Seconds(), ttl.Seconds(), 5)

	require.NoError(t, f.flow.ApplyOrderEvent(ctx, s.Schema, s.ID,
		fixtureOrderUUID, django.PaymentStatusCompleted))
	assert.Equal(t, checkout.StatusCompleted, f.stored(t, s).Status)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load())
}

// An event can come long after its order: a payment made days later, or
// Django pushing again. The session's day counts from the order, so one
// recorded by such an event has already ended.
func TestOrderEventForAnOldOrderDoesNotKeepItsSession(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	ctx := context.Background()
	s := f.readySession(t)
	s.Status = checkout.StatusCompleteInProgress
	require.NoError(t, f.store.Save(ctx, s))

	// The fixture order was created months ago.
	require.NoError(t, f.flow.ApplyOrderEvent(ctx, s.Schema, s.ID,
		fixtureOrderUUID, django.PaymentStatusCompleted))

	_, err := f.store.Load(ctx, s.Schema, s.ID)
	assert.ErrorIs(t, err, checkout.ErrNotFound)
}

// A session that is gone before its completion's last save cannot be
// stored again. The order is placed: that is reported as a placed order
// whose session was not saved, with the key spent, and never as a
// session that changed, which a caller answers "nothing happened, send
// the request again".
func TestCompleteReportsAPlacedOrderWhoseSessionIsGone(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	ctx := context.Background()
	s := f.readySession(t)
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if !setsKey(cmd, ":orderlink:") {
			return nil
		}
		return f.rdb.Del(ctx, sessionRedisKey(s.ID)).Err()
	}))

	placement, err := f.flow.Complete(ctx, f.tenant, s)

	require.ErrorIs(t, err, checkout.ErrSessionNotSaved)
	assert.NotErrorIs(t, err, checkout.ErrSessionChanged)
	assert.Equal(t, checkout.OrderMayExist, placement)
	assert.Equal(t, fixtureOrderUUID, s.OrderUUID)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load())
}

// The in-progress save is what guards the order. When Redis refuses it
// no order is sent, and the session is not written again: that write
// would be a ready snapshot saved over whatever the store holds by then.
func TestCompleteSendsNoOrderWithoutTheInProgressSave(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	redisDown := errors.New("redis is down")
	// The revision each session write would store (the script's ARGV[2]).
	var revisions []any
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if !savesKey(cmd, sessionRedisKey(s.ID)) {
			return nil
		}
		revisions = append(revisions, cmd.Args()[6])
		return redisDown
	}))

	placement, err := f.flow.Complete(context.Background(), f.tenant, s)

	require.ErrorIs(t, err, redisDown)
	assert.Equal(t, checkout.NothingPlaced, placement)
	// The mark and its second attempt, one write sent twice, and no
	// write after them.
	require.Len(t, revisions, 2)
	assert.Equal(t, revisions[0], revisions[1])
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
		case linked && setsKey(cmd, sessionRedisKey(s.ID)):
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
