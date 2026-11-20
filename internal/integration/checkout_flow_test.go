//go:build integration

package integration

import (
	"context"
	"encoding/json"
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

// testOrderRetry sends an unanswered order up to three times, quickly,
// as the gateway does (config.OrderCreateAttempts).
var testOrderRetry = checkout.OrderRetry{
	Attempts: 3, Delay: 10 * time.Millisecond,
}

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
			testOrderRetry, quietLogger()),
		store: store, rdb: rdb, faults: faults,
		tenant: &tenant.Tenant{
			SchemaName: "demostore", DefaultLocale: "el",
			DefaultCurrency: "EUR", Domain: "shop.test",
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
	// What its last render showed: the fixture cart, delivered free and
	// paid in cash without a fee.
	s.ShownTotal = &fixtureShownTotal
	require.NoError(t, f.store.Create(t.Context(), s,
		checkout.CartHolder{}))
	return s
}

// fixtureShownTotal is the total a checkout of the fixture cart shows with
// ACS home delivery and cash on delivery: its items, as Django quotes them.
var fixtureShownTotal = django.ExpectedTotal{Amount: 92936, Currency: "EUR"}

func (f flowStack) stored(t *testing.T, s *checkout.Session) *checkout.Session {
	t.Helper()
	got, err := f.store.LoadAnyProtocol(t.Context(), s.Schema, s.ID)
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
	ctx, cancel := context.WithCancel(t.Context())
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
		t.Context(), s.Schema, fixtureOrderUUID)
	require.NoError(t, err, "the order link exists")
	assert.Equal(t, s.ID, link.CheckoutID)
}

// A caller that gives up while Django is still creating the order does
// not end the creation: Django answers, and the checkout is saved as
// placed.
func TestCompleteFinishesAnOrderAnsweredAfterTheCallerLeft(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	ctx, cancel := context.WithCancel(t.Context())
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
		t.Context(), s.Schema, fixtureOrderUUID)
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
	ctx, cancel := context.WithCancel(t.Context())
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
	require.NotErrorIs(t, err, context.Canceled,
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

	placement, err := f.flow.Complete(t.Context(), f.tenant, s)

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
		// The store's quote refuses it before the pay ways are read.
		{"a pay way the store does not offer", func(s *checkout.Session) {
			s.PayWayID = 99
		}, checkout.ErrChoiceRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := f.readySession(t)
			tc.edit(s)
			s.Buyer.FirstName = "unsaved"

			placement, err := f.flow.Complete(
				t.Context(), f.tenant, s)

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
	ctx := t.Context()
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
	ctx := t.Context()
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
		require.ErrorIs(t, o.err, checkout.ErrSessionChanged)
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
	ttl, err := f.rdb.PTTL(t.Context(),
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
				t.Context(), f.tenant, s)

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
	ctx := t.Context()
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
	ctx := t.Context()
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

	placement, err := f.flow.Complete(t.Context(), f.tenant, s)

	require.True(t, lost.Load(), "the mark's reply was lost")
	require.NoError(t, err)
	assert.Equal(t, checkout.OrderMayExist, placement)
	assert.Equal(t, checkout.StatusCompleted, f.stored(t, s).Status)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load())
}

// fenceRepliesLost lets every attempt of the write that marks session id
// in progress reach Redis and loses its reply, and counts them.
func fenceRepliesLost(rdb *redis.Client, id string) *atomic.Int32 {
	var lost atomic.Int32
	var fence atomic.Pointer[string]
	rdb.AddHook(replyLost(func(cmd redis.Cmder) bool {
		if !savesKey(cmd, sessionRedisKey(id)) {
			return false
		}
		raw, _ := cmd.Args()[7].([]byte)
		var written checkout.Session
		if json.Unmarshal(raw, &written) != nil {
			return false
		}
		if written.Status == checkout.StatusCompleteInProgress {
			fence.CompareAndSwap(nil, &written.Revision)
		}
		if mark := fence.Load(); mark == nil || *mark != written.Revision {
			return false
		}
		lost.Add(1)
		return true
	}))
	return &lost
}

// The mark can be stored and lose its reply on both attempts. The
// completion reads the session once: it holds the mark's revision, which
// no other write can, so the mark is this call's and the order is placed
// once, not left marked with nothing sent until the session expires.
func TestAMarkWhoseRepliesWereBothLostPlacesTheOrder(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	lost := fenceRepliesLost(f.rdb, s.ID)

	placement, err := f.flow.Complete(t.Context(), f.tenant, s)

	require.EqualValues(t, 2, lost.Load(), "both replies were lost")
	require.NoError(t, err)
	assert.Equal(t, checkout.OrderMayExist, placement)
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusCompleted, stored.Status)
	assert.Equal(t, fixtureOrderUUID, stored.OrderUUID)
	assert.Equal(t, s.Revision, stored.Revision)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load())
}

// When the read after both lost replies fails too, the completion cannot
// tell its mark was stored: it sends no order, as before the read.
func TestAMarkThatCannotBeReadBackSendsNoOrder(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	lost := fenceRepliesLost(f.rdb, s.ID)
	var refused atomic.Bool
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if args := cmd.Args(); cmd.Name() == "get" && len(args) > 1 &&
			args[1] == sessionRedisKey(s.ID) && lost.Load() == 2 &&
			refused.CompareAndSwap(false, true) {
			return errors.New("redis is down")
		}
		return nil
	}))

	placement, err := f.flow.Complete(t.Context(), f.tenant, s)

	require.True(t, refused.Load(), "the session was not read back")
	require.ErrorIs(t, err, checkout.ErrUnavailable)
	assert.Equal(t, checkout.NothingPlaced, placement)
	assert.Zero(t, f.faults.orderCreates.Load())
	assert.Equal(t, checkout.StatusCompleteInProgress, f.stored(t, s).Status)
}

// A read-back that starts just before the completion's budget ends, of a
// Redis gone silent, is cut at the budget's deadline: the completion
// cannot tell its mark was stored and sends no order. Read after the
// deadline instead, it would find the mark and try the order on a
// context already over, which sends nothing, and answer an order never
// sent as one whose outcome is unknown, with its key spent.
func TestAMarkReadBackPastTheBudgetSendsNoOrder(t *testing.T) {
	const upstream = 500 * time.Millisecond
	f := startFlow(t, upstream)
	s := f.readySession(t)
	lost := fenceRepliesLost(f.rdb, s.ID)
	// The hook runs on the flow's goroutine: it records what it did and
	// asserts nothing.
	var paused, injected atomic.Bool
	var fault atomic.Pointer[string]
	f.rdb.AddHook(redisAt(
		func(ctx context.Context, cmd redis.Cmder, send func() error) error {
			if args := cmd.Args(); cmd.Name() != "get" || len(args) < 2 ||
				args[1] != sessionRedisKey(s.ID) || lost.Load() != 2 ||
				!paused.CompareAndSwap(false, true) {
				return send()
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				msg := "the read-back has no deadline"
				fault.Store(&msg)
				return send()
			}
			time.Sleep(time.Until(deadline) - 100*time.Millisecond)
			if err := pauseRedis(t, f.rdb, time.Second); err != nil {
				msg := "pausing Redis failed: " + err.Error()
				fault.Store(&msg)
				return send()
			}
			injected.Store(true)
			return send()
		}))

	began := time.Now()
	placement, err := f.flow.Complete(t.Context(), f.tenant, s)
	took := time.Since(began)

	require.True(t, paused.Load(), "the session was not read back")
	if msg := fault.Load(); msg != nil {
		require.Fail(t, *msg)
	}
	require.True(t, injected.Load(), "Redis was not paused before the read")
	require.ErrorIs(t, err, checkout.ErrUnavailable)
	assert.Equal(t, checkout.NothingPlaced, placement)
	assert.Zero(t, f.faults.orderCreates.Load())
	assert.Less(t, took, checkout.CompletionBudget(upstream)+
		200*time.Millisecond, "the read-back outlived the budget")
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

	placement, err := f.flow.Complete(t.Context(), f.tenant, s)

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
	ctx := t.Context()
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
			fixtureOrderUUID, "PROCESSING", django.PaymentStatusCompleted)
	}))

	placement, err := f.flow.Complete(ctx, f.tenant, s)

	require.NoError(t, err)
	assert.Equal(t, checkout.OrderMayExist, placement)
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusCompleted, stored.Status)
	assert.Equal(t, fixtureOrderUUID, stored.OrderUUID)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load())
}

// An order event stored while the completion that placed its order was
// still running published an expiry, a day from the order: the
// completion's own save over it keeps that expiry, never an earlier one.
func TestCompleteOverAnOrderEventKeepsTheExpiryItPublished(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	ctx := t.Context()
	f.tenant.AgentCommerceEnabled = true
	f.tenant.AgentHostedPaymentEnabled = true
	s := f.readySession(t)
	s.PayWayID = 2
	require.NoError(t, f.store.Save(ctx, s))
	placed := time.Now()
	f.faults.orderCreatedAt.Store(&placed)
	var published time.Time
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if !setsKey(cmd, ":orderlink:") || !published.IsZero() {
			return nil
		}
		err := f.flow.ApplyOrderEvent(ctx, s.Schema, s.ID,
			fixtureOrderUUID, "PENDING", "PENDING")
		if err == nil {
			published = f.stored(t, s).ExpiresAt
		}
		return err
	}))

	_, err := f.flow.Complete(ctx, f.tenant, s)

	require.NoError(t, err)
	require.False(t, published.IsZero(), "the event was stored")
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusRequiresEscalation, stored.Status)
	assert.False(t, stored.ExpiresAt.Before(published),
		"%v is earlier than %v", stored.ExpiresAt, published)
}

// A completion that placed its order and could not save leaves a session
// in progress that does not name the order. An order event finds it
// through the order link and records the order, read from Django by the
// event's uuid: its id, and its creation time, from which the session's
// day counts. A payment event then completes it.
func TestOrderEventRecordsTheOrderItsSessionNeverSaved(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	ctx := t.Context()
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
		fixtureOrderUUID, "PENDING", "PENDING"))

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
		fixtureOrderUUID, "PROCESSING", django.PaymentStatusCompleted))
	assert.Equal(t, checkout.StatusCompleted, f.stored(t, s).Status)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load())
}

// An event can come long after its order: a payment made days later, or
// Django pushing again. The session's day counts from the order, so one
// recorded by such an event is not kept a day after the event: it ends
// when the expiry it already published does.
func TestOrderEventForAnOldOrderDoesNotKeepItsSession(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	ctx := t.Context()
	s := f.readySession(t)
	s.Status = checkout.StatusCompleteInProgress
	require.NoError(t, f.store.Save(ctx, s))
	published := s.ExpiresAt

	// The fixture order was created months ago.
	require.NoError(t, f.flow.ApplyOrderEvent(ctx, s.Schema, s.ID,
		fixtureOrderUUID, "PROCESSING", django.PaymentStatusCompleted))

	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusCompleted, stored.Status)
	assert.True(t, stored.ExpiresAt.Equal(published), "%v", stored.ExpiresAt)
	ttl, err := f.rdb.PTTL(ctx, sessionRedisKey(s.ID)).Result()
	require.NoError(t, err)
	assert.LessOrEqual(t, ttl, time.Until(published)+time.Second)
}

// A session that is gone before its completion's last save cannot be
// stored again. The order is placed: that is reported as a placed order
// whose session was not saved, with the key spent, and never as a
// session that changed, which a caller answers "nothing happened, send
// the request again".
func TestCompleteReportsAPlacedOrderWhoseSessionIsGone(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	ctx := t.Context()
	s := f.readySession(t)
	f.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if !setsKey(cmd, ":orderlink:") {
			return nil
		}
		return f.rdb.Del(ctx, sessionRedisKey(s.ID)).Err()
	}))

	placement, err := f.flow.Complete(ctx, f.tenant, s)

	require.ErrorIs(t, err, checkout.ErrSessionNotSaved)
	require.NotErrorIs(t, err, checkout.ErrSessionChanged)
	assert.Equal(t, checkout.OrderMayExist, placement)
	assert.Equal(t, fixtureOrderUUID, s.OrderUUID)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load())
}

// The in-progress save is what guards the order. When Redis refuses it
// no order is sent, and the session is not written again: that write
// would be a ready snapshot saved over whatever the store holds by then.
// The read after the second failure finds the session as loaded.
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

	placement, err := f.flow.Complete(t.Context(), f.tenant, s)

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

	placement, err := f.flow.Complete(t.Context(), f.tenant, s)

	require.ErrorIs(t, err, checkout.ErrSessionNotSaved)
	assert.Equal(t, checkout.OrderMayExist, placement)
	assert.Equal(t, fixtureOrderUUID, s.OrderUUID)
	assert.Equal(t, checkout.StatusCompleteInProgress, f.stored(t, s).Status)
	link, err := f.store.OrderLinkFor(
		t.Context(), s.Schema, fixtureOrderUUID)
	require.NoError(t, err, "the order link exists")
	assert.Equal(t, s.ID, link.CheckoutID)
}

// An order whose answer was lost is sent again with the checkout's id,
// and the store answers the order it placed: the checkout completes with
// that one order.
func TestCompleteSendsAnUnansweredOrderAgainAndKeepsTheOne(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	f.faults.orderAnswerLost.Store(1)

	placement, err := f.flow.Complete(t.Context(), f.tenant, s)

	require.NoError(t, err)
	assert.Equal(t, checkout.OrderMayExist, placement)
	assert.EqualValues(t, 2, f.faults.orderCreates.Load())
	assert.EqualValues(t, 1, f.faults.ordersPlaced.Load())
	assert.Equal(t, s.ID, (*f.faults.lastOrder.Load())["agentCheckout"])
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusCompleted, stored.Status)
	assert.Equal(t, fixtureOrderUUID, stored.OrderUUID)
	assert.False(t, stored.Unconfirmed)
}

// The order is sent at most as often as the retry policy allows, and a
// store that never answers leaves the checkout unconfirmed.
func TestCompleteSendsTheOrderAsOftenAsItsPolicyAllows(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	f.faults.orderCreate.Store(true)

	placement, err := f.flow.Complete(t.Context(), f.tenant, s)

	require.ErrorIs(t, err, checkout.ErrOrderOutcomeUnknown)
	assert.Equal(t, checkout.OrderMayExist, placement)
	assert.EqualValues(t, testOrderRetry.Attempts,
		f.faults.orderCreates.Load())
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusCompleteInProgress, stored.Status)
	assert.True(t, stored.Unconfirmed)
	assert.True(t, f.flow.Resumable(stored))
}

// A refusal is the store's answer: nothing was created, and it is not
// sent again.
func TestCompleteDoesNotSendARefusedOrderAgain(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.readySession(t)
	f.faults.orderRefuse.Store(true)

	placement, err := f.flow.Complete(t.Context(), f.tenant, s)

	require.ErrorIs(t, err, django.ErrValidation)
	assert.Equal(t, checkout.NothingPlaced, placement)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load())
}

// unconfirmed leaves s stored as a completion whose every send went
// unanswered: placed is whether the store took the first one.
func (f flowStack) unconfirmed(
	t *testing.T, s *checkout.Session, placed bool,
) *checkout.Session {
	t.Helper()
	if placed {
		f.faults.orderAnswerLost.Store(1)
		lost := func(*http.Request) { f.faults.orderCreate.Store(true) }
		f.faults.orderHold.Store(&lost)
	} else {
		f.faults.orderCreate.Store(true)
	}
	_, err := f.flow.Complete(t.Context(), f.tenant, s)
	require.ErrorIs(t, err, checkout.ErrOrderOutcomeUnknown)
	f.faults.orderHold.Store(nil)
	f.faults.orderCreate.Store(false)
	stored := f.stored(t, s)
	require.True(t, stored.Unconfirmed)
	return stored
}

// A complete of a checkout whose order went unanswered asks the store
// again with its id. The store answers the order the checkout placed, or
// places it now if the first never reached it: one order either way, and
// the checkout ends completed.
func TestResumeEndsAnUnconfirmedCheckoutWithItsOneOrder(t *testing.T) {
	for _, placed := range []bool{true, false} {
		t.Run(fmt.Sprintf("the store had placed it: %t", placed),
			func(t *testing.T) {
				f := startFlow(t, 5*time.Second)
				s := f.unconfirmed(t, f.readySession(t), placed)
				creates := f.faults.orderCreates.Load()

				placement, err := f.flow.Resume(t.Context(), f.tenant, s)

				require.NoError(t, err)
				assert.Equal(t, checkout.OrderMayExist, placement)
				assert.EqualValues(t, 1, f.faults.orderCreates.Load()-creates)
				assert.EqualValues(t, 1, f.faults.ordersPlaced.Load())
				stored := f.stored(t, s)
				assert.Equal(t, checkout.StatusCompleted, stored.Status)
				assert.Equal(t, fixtureOrderUUID, stored.OrderUUID)
				assert.False(t, stored.Unconfirmed)
				assert.False(t, f.flow.Resumable(stored))
			})
	}
}

// The store refuses an order the checkout never placed: the checkout is
// ready again, and says nothing of an order.
func TestResumeThatTheStoreRefusesLeavesTheCheckoutReady(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.unconfirmed(t, f.readySession(t), false)
	f.faults.orderRefuse.Store(true)

	placement, err := f.flow.Resume(t.Context(), f.tenant, s)

	require.ErrorIs(t, err, django.ErrValidation)
	assert.Equal(t, checkout.NothingPlaced, placement)
	stored := f.stored(t, s)
	assert.Equal(t, checkout.StatusReadyForComplete, stored.Status)
	assert.False(t, stored.Unconfirmed)
	assert.Zero(t, f.faults.ordersPlaced.Load())
}

// A Resume that cannot read how the order is paid sends nothing: the
// checkout stays unconfirmed for the next complete.
func TestResumeThatCannotReadThePayWaySendsNothing(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.unconfirmed(t, f.readySession(t), false)
	creates := f.faults.orderCreates.Load()
	before := f.stored(t, s)
	f.faults.payWayDetail.Store(true)

	placement, err := f.flow.Resume(t.Context(), f.tenant, s)

	require.ErrorIs(t, err, checkout.ErrOrderOutcomeUnknown)
	assert.Equal(t, checkout.NothingPlaced, placement)
	assert.Equal(t, creates, f.faults.orderCreates.Load())
	assert.Equal(t, before, f.stored(t, s))
}

// A session whose completion may still be running is not resumed, and
// of two calls that loaded one resumable session, one sends the order.
func TestResumeIsOneCallAndNeverOverARunningCompletion(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	s := f.unconfirmed(t, f.readySession(t), true)
	other := f.stored(t, s)
	creates := f.faults.orderCreates.Load()

	_, err := f.flow.Resume(t.Context(), f.tenant, s)
	require.NoError(t, err)
	placement, err := f.flow.Resume(t.Context(), f.tenant, other)
	require.ErrorIs(t, err, checkout.ErrSessionChanged)
	assert.Equal(t, checkout.NothingPlaced, placement)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load()-creates)

	running := f.readySession(t)
	running.Status = checkout.StatusCompleteInProgress
	running.UpdatedAt = time.Now()
	require.False(t, f.flow.Resumable(running))
	placement, err = f.flow.Resume(t.Context(), f.tenant, running)
	require.ErrorIs(t, err, checkout.ErrCompletionInProgress)
	assert.Equal(t, checkout.NothingPlaced, placement)
	assert.EqualValues(t, 1, f.faults.orderCreates.Load()-creates)
}
