//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// Where the gateway names the session that holds a cart.
const cartRecordPrefix = "ag:demostore:checkoutcart:"

// heldBy is the id of the session a cart's record names, "" for a cart
// without a record.
func heldBy(t *testing.T, rdb *redis.Client, cartID string) string {
	t.Helper()
	id, err := rdb.Get(context.Background(), cartRecordPrefix+cartID).Result()
	if !errors.Is(err, redis.Nil) {
		require.NoError(t, err)
	}
	return id
}

// sessionOn is another session on the cart of an existing one.
func sessionOn(cartID string) *checkout.Session {
	return checkout.NewSession("demostore", "shop.test",
		checkout.ProtocolUCP, cartID)
}

// assertLivesWithSession checks that a cart's record ends when the
// session it names does, want from now.
func assertLivesWithSession(
	t *testing.T, rdb *redis.Client, s *checkout.Session, want time.Duration,
) {
	t.Helper()
	ctx := context.Background()
	session, err := rdb.PTTL(ctx, sessionRedisKey(s.ID)).Result()
	require.NoError(t, err)
	record, err := rdb.PTTL(ctx, cartRecordPrefix+s.CartID).Result()
	require.NoError(t, err)
	assert.InDelta(t, want.Seconds(), session.Seconds(), 5, "the session")
	assert.InDelta(t, session.Seconds(), record.Seconds(), 1,
		"the cart's record does not end with its session")
}

// staleCartRecords lists the cart records that name a session the store
// no longer holds.
func (stack ucpStack) staleCartRecords(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	keys, err := stack.rdb.Keys(ctx, cartRecordPrefix+"*").Result()
	require.NoError(t, err)
	var stale []string
	for _, key := range keys {
		id, err := stack.rdb.Get(ctx, key).Result()
		require.NoError(t, err)
		n, err := stack.rdb.Exists(ctx, sessionRedisKey(id)).Result()
		require.NoError(t, err)
		if n == 0 {
			stale = append(stale, key)
		}
	}
	return stale
}

// A session is stored with its cart: the cart's record names it, and no
// second session is stored on that cart while the first holds it. Two
// sessions on one cart could each place an order.
func TestACartIsTakenWithItsSession(t *testing.T) {
	ctx := context.Background()
	store, rdb := startStore(t)
	first := newSession()
	require.NoError(t, store.Create(ctx, first, checkout.CartHolder{}))
	assert.Equal(t, first.ID, heldBy(t, rdb, first.CartID))

	refused := func(t *testing.T, second *checkout.Session) {
		t.Helper()
		_, err := store.Load(ctx, second.Schema, second.ID)
		assert.ErrorIs(t, err, checkout.ErrNotFound,
			"a refused create stored its session")
		assert.Empty(t, second.Revision)
		assert.Equal(t, first.ID, heldBy(t, rdb, first.CartID))
	}

	t.Run("a create that saw no holder is refused", func(t *testing.T) {
		second := sessionOn(first.CartID)
		require.ErrorIs(t, store.Create(ctx, second, checkout.CartHolder{}),
			checkout.ErrCartHeld)
		refused(t, second)
	})

	// Having read the holder gives no right to its cart: only a holder
	// that is free may be replaced.
	t.Run("a create that saw the open holder is refused", func(t *testing.T) {
		seen, err := store.HolderOf(ctx, first.Schema, first.CartID)
		require.NoError(t, err)
		require.NotNil(t, seen.Session)
		assert.Equal(t, first.ID, seen.Session.ID)
		assert.False(t, seen.Free())
		second := sessionOn(first.CartID)
		require.ErrorIs(t, store.Create(ctx, second, seen),
			checkout.ErrCartHeld)
		refused(t, second)
	})

	// The session's own create, sent again after the session was lost:
	// the cart is still its own.
	t.Run("a session takes the cart it already holds", func(t *testing.T) {
		s := newSession()
		require.NoError(t, store.Create(ctx, s, checkout.CartHolder{}))
		require.NoError(t, rdb.Del(ctx, sessionRedisKey(s.ID)).Err())
		require.NoError(t, store.Create(ctx, s, checkout.CartHolder{}))
		assert.Equal(t, s.ID, heldBy(t, rdb, s.CartID))
	})
}

// Who holds a cart is decided on the holding session's state: an open
// one keeps it, and so does one whose order is being placed or awaits
// its payment. A canceled or completed one, and a record whose session
// is gone, leave the cart to the next checkout.
func TestWhoHoldsACart(t *testing.T) {
	ctx := context.Background()
	store, rdb := startStore(t)
	for name, tc := range map[string]struct {
		become func(*checkout.Session)
		free   bool
	}{
		"incomplete": {func(*checkout.Session) {}, false},
		"ready": {func(s *checkout.Session) {
			s.Status = checkout.StatusReadyForComplete
		}, false},
		"its order being placed": {func(s *checkout.Session) {
			s.Status = checkout.StatusCompleteInProgress
		}, false},
		"its order awaiting payment": {func(s *checkout.Session) {
			s.Status = checkout.StatusRequiresEscalation
			s.OrderUUID, s.PlacedAt = fixtureOrderUUID, time.Now()
		}, false},
		"completed": {func(s *checkout.Session) {
			s.Status = checkout.StatusCompleted
			s.OrderUUID, s.PlacedAt = fixtureOrderUUID, time.Now()
		}, true},
		"canceled": {func(s *checkout.Session) {
			s.Status = checkout.StatusCanceled
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			holder := newSession()
			require.NoError(t, store.Create(ctx, holder, checkout.CartHolder{}))
			tc.become(holder)
			require.NoError(t, store.Save(ctx, holder))

			seen, err := store.HolderOf(ctx, holder.Schema, holder.CartID)
			require.NoError(t, err)
			require.NotNil(t, seen.Session)
			assert.Equal(t, holder.ID, seen.Session.ID)
			assert.Equal(t, holder.Status, seen.Session.Status)
			assert.Equal(t, tc.free, seen.Free())

			next := sessionOn(holder.CartID)
			err = store.Create(ctx, next, seen)
			if !tc.free {
				require.ErrorIs(t, err, checkout.ErrCartHeld)
				assert.Equal(t, holder.ID, heldBy(t, rdb, holder.CartID))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, next.ID, heldBy(t, rdb, holder.CartID))
			// The old holder is still read by its own id.
			kept, err := store.Load(ctx, holder.Schema, holder.ID)
			require.NoError(t, err)
			assert.Equal(t, holder.Revision, kept.Revision)
		})
	}

	t.Run("a cart without a record", func(t *testing.T) {
		seen, err := store.HolderOf(ctx, "demostore", uuid.NewString())
		require.NoError(t, err)
		assert.Nil(t, seen.Session)
		assert.True(t, seen.Free())
	})

	t.Run("a record whose session is gone", func(t *testing.T) {
		holder := newSession()
		require.NoError(t, store.Create(ctx, holder, checkout.CartHolder{}))
		require.NoError(t, rdb.Del(ctx, sessionRedisKey(holder.ID)).Err())

		seen, err := store.HolderOf(ctx, holder.Schema, holder.CartID)
		require.NoError(t, err)
		assert.Nil(t, seen.Session)
		assert.True(t, seen.Free())
		// A create that never read the record does not replace it.
		blind := sessionOn(holder.CartID)
		require.ErrorIs(t, store.Create(ctx, blind, checkout.CartHolder{}),
			checkout.ErrCartHeld)
		next := sessionOn(holder.CartID)
		require.NoError(t, store.Create(ctx, next, seen))
		assert.Equal(t, next.ID, heldBy(t, rdb, holder.CartID))
	})

	// Two creates read one free holder. The first takes the cart; the
	// second expected the holder it read, finds another, and is refused.
	t.Run("a cart taken since it was read", func(t *testing.T) {
		holder := newSession()
		require.NoError(t, store.Create(ctx, holder, checkout.CartHolder{}))
		holder.Status = checkout.StatusCanceled
		require.NoError(t, store.Save(ctx, holder))
		seen, err := store.HolderOf(ctx, holder.Schema, holder.CartID)
		require.NoError(t, err)

		winner, loser := sessionOn(holder.CartID), sessionOn(holder.CartID)
		require.NoError(t, store.Create(ctx, winner, seen))
		require.ErrorIs(t, store.Create(ctx, loser, seen),
			checkout.ErrCartHeld)
		assert.Equal(t, winner.ID, heldBy(t, rdb, holder.CartID))
		_, err = store.Load(ctx, loser.Schema, loser.ID)
		assert.ErrorIs(t, err, checkout.ErrNotFound)
	})
}

// A cart's record ends when its session does: every save of the session
// gives the record the session's lifetime, a placement included. A
// record that outlived its session would be stale; one that ended first
// would free the cart of a live checkout.
func TestACartsRecordLivesAsLongAsItsSession(t *testing.T) {
	ctx := context.Background()
	const day, halfHour = 24 * time.Hour, 30 * time.Minute

	t.Run("through a save", func(t *testing.T) {
		store, rdb := startStore(t)
		s := newSession()
		require.NoError(t, store.Create(ctx, s, checkout.CartHolder{}))
		assertLivesWithSession(t, rdb, s, halfHour)

		s.Status = checkout.StatusRequiresEscalation
		s.OrderUUID, s.PlacedAt = fixtureOrderUUID, time.Now()
		require.NoError(t, store.Save(ctx, s))
		assertLivesWithSession(t, rdb, s, day)
	})

	t.Run("through a placement", func(t *testing.T) {
		f := startFlow(t, 5*time.Second)
		s := f.readySession(t)
		assertLivesWithSession(t, f.rdb, s, halfHour)

		placement, err := f.flow.Complete(ctx, f.tenant, s)
		require.NoError(t, err)
		require.Equal(t, checkout.OrderMayExist, placement)
		require.Equal(t, checkout.StatusCompleted, f.stored(t, s).Status)
		assert.Equal(t, s.ID, heldBy(t, f.rdb, s.CartID))
		assertLivesWithSession(t, f.rdb, s, day)
	})

	// Redis evicts keys one by one, so a live session can lose its
	// cart's record. Its next save writes it again.
	t.Run("a save writes again a record Redis lost", func(t *testing.T) {
		store, rdb := startStore(t)
		s := newSession()
		require.NoError(t, store.Create(ctx, s, checkout.CartHolder{}))
		require.NoError(t, rdb.Del(ctx, cartRecordPrefix+s.CartID).Err())

		require.NoError(t, store.Save(ctx, s))

		assert.Equal(t, s.ID, heldBy(t, rdb, s.CartID))
		assertLivesWithSession(t, rdb, s, halfHour)
	})

	// A session that lost its cart to the next checkout leaves that
	// checkout's record alone, whatever it saves.
	t.Run("a save of a session that no longer holds the cart",
		func(t *testing.T) {
			store, rdb := startStore(t)
			old := newSession()
			require.NoError(t, store.Create(ctx, old, checkout.CartHolder{}))
			old.Status = checkout.StatusCanceled
			require.NoError(t, store.Save(ctx, old))
			seen, err := store.HolderOf(ctx, old.Schema, old.CartID)
			require.NoError(t, err)
			next := sessionOn(old.CartID)
			require.NoError(t, store.Create(ctx, next, seen))

			require.NoError(t, store.Save(ctx, old))

			assert.Equal(t, next.ID, heldBy(t, rdb, old.CartID))
			assertLivesWithSession(t, rdb, next, halfHour)
		})
}

// A session that ends takes its cart's record with it, and only its
// own: by then the record may name the next checkout on the cart.
func TestAnEndingSessionFreesOnlyItsOwnCart(t *testing.T) {
	ctx := context.Background()
	store, rdb := startStore(t)
	// over is a session whose cart went to next once it completed.
	over := func(t *testing.T) (old, next *checkout.Session) {
		t.Helper()
		old = newSession()
		old.Status = checkout.StatusCompleted
		old.PlacedAt = time.Now().Add(-23 * time.Hour)
		require.NoError(t, store.Create(ctx, old, checkout.CartHolder{}))
		seen, err := store.HolderOf(ctx, old.Schema, old.CartID)
		require.NoError(t, err)
		next = sessionOn(old.CartID)
		require.NoError(t, store.Create(ctx, next, seen))
		return old, next
	}

	t.Run("a discarded session frees its cart", func(t *testing.T) {
		s := newSession()
		require.NoError(t, store.Create(ctx, s, checkout.CartHolder{}))
		require.NoError(t, store.Discard(ctx, s))
		assert.Empty(t, heldBy(t, rdb, s.CartID))
		again := sessionOn(s.CartID)
		require.NoError(t, store.Create(ctx, again, checkout.CartHolder{}))
	})

	t.Run("a discarded session leaves the next holder's record",
		func(t *testing.T) {
			old, next := over(t)
			require.NoError(t, store.Discard(ctx, old))
			assert.Equal(t, next.ID, heldBy(t, rdb, old.CartID))
		})

	t.Run("a placed session past its day frees its cart", func(t *testing.T) {
		s := newSession()
		s.Status = checkout.StatusCompleted
		s.PlacedAt = time.Now().Add(-23 * time.Hour)
		require.NoError(t, store.Create(ctx, s, checkout.CartHolder{}))
		require.Equal(t, s.ID, heldBy(t, rdb, s.CartID))

		s.PlacedAt = time.Now().Add(-25 * time.Hour)
		require.NoError(t, store.Save(ctx, s))

		_, err := store.Load(ctx, s.Schema, s.ID)
		require.ErrorIs(t, err, checkout.ErrNotFound)
		assert.Empty(t, heldBy(t, rdb, s.CartID))
	})

	t.Run("a placed session past its day leaves the next holder's record",
		func(t *testing.T) {
			old, next := over(t)
			old.PlacedAt = time.Now().Add(-25 * time.Hour)
			require.NoError(t, store.Save(ctx, old))

			_, err := store.Load(ctx, old.Schema, old.ID)
			require.ErrorIs(t, err, checkout.ErrNotFound)
			assert.Equal(t, next.ID, heldBy(t, rdb, old.CartID))
		})
}

// go-redis sends a command again when its reply was lost. A create that
// took its cart from an old holder finds, on its second run, the cart in
// its own name and its own revision stored, and reports the work done.
func TestACartCommandSentTwiceIsAnsweredAsDone(t *testing.T) {
	ctx := context.Background()
	store, rdb := startStore(t)
	twice := new(sentTwice)
	rdb.AddHook(twice)

	old := newSession()
	require.NoError(t, store.Create(ctx, old, checkout.CartHolder{}))
	old.Status = checkout.StatusCanceled
	require.NoError(t, store.Save(ctx, old))
	seen, err := store.HolderOf(ctx, old.Schema, old.CartID)
	require.NoError(t, err)

	next := sessionOn(old.CartID)
	twice.during(t, func() { err = store.Create(ctx, next, seen) })
	require.NoError(t, err, "a create over an old holder")
	assert.Equal(t, next.ID, heldBy(t, rdb, old.CartID))
	stored, err := store.Load(ctx, next.Schema, next.ID)
	require.NoError(t, err)
	assert.Equal(t, next.Revision, stored.Revision)

	twice.during(t, func() { err = store.Discard(ctx, next) })
	require.NoError(t, err, "a discard")
	assert.Empty(t, heldBy(t, rdb, old.CartID))
}

// platform calls the UCP checkout tools as one platform.
type platform struct {
	session *mcp.ClientSession
	profile string
}

func newPlatform(t *testing.T, stack ucpStack) platform {
	t.Helper()
	sink := httptest.NewServer(newWebhookSink().handler())
	t.Cleanup(sink.Close)
	return platform{
		session: connectMCP(t, stack.gw.URL),
		profile: servePlatformProfile(t, sink.URL+"/ucp/orders", nil),
	}
}

func (p platform) meta(idempotencyKey string) map[string]any {
	m := map[string]any{"ucp-agent": map[string]any{"profile": p.profile}}
	if idempotencyKey != "" {
		m["idempotency-key"] = idempotencyKey
	}
	return m
}

// createAnswer is how create_checkout answered: the checkout it gave and
// the text beside it, or its refusal.
type createAnswer struct {
	id, text, refusal string
}

// create calls create_checkout for a cart, or with no cart for the line
// items submit carries. It asserts nothing, so it runs on any goroutine.
func (p platform) create(
	ctx context.Context, cartID string, submit map[string]any,
) createAnswer {
	if submit == nil {
		submit = map[string]any{}
	}
	args := map[string]any{"meta": p.meta(""), "checkout": submit}
	if cartID != "" {
		args["cart_id"] = cartID
	}
	res, err := p.session.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_checkout", Arguments: args,
	})
	if err != nil {
		return createAnswer{refusal: err.Error()}
	}
	var text string
	if len(res.Content) > 0 {
		if content, ok := res.Content[0].(*mcp.TextContent); ok {
			text = content.Text
		}
	}
	if res.IsError {
		return createAnswer{refusal: "refused: " + text}
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		return createAnswer{refusal: "no structured checkout: " + text}
	}
	id, _ := out["id"].(string)
	return createAnswer{id: id, text: text}
}

// started is the id of a checkout create_checkout made on the cart.
func (p platform) started(t *testing.T, cartID string) string {
	t.Helper()
	a := p.create(context.Background(), cartID, nil)
	require.Empty(t, a.refusal)
	require.NotEmpty(t, a.id)
	return a.id
}

// become stores a checkout as change leaves it: the state another call
// would have brought it to.
func (stack ucpStack) become(
	t *testing.T, id string, change func(*checkout.Session),
) {
	t.Helper()
	ctx := context.Background()
	store := checkout.NewStore(stack.rdb, ucpUpstreamTimeout)
	s, err := store.Load(ctx, "demostore", id)
	require.NoError(t, err)
	change(s)
	require.NoError(t, store.Save(ctx, s))
}

// sessions are the checkout sessions the store holds.
func (stack ucpStack) sessions(t *testing.T) []*checkout.Session {
	t.Helper()
	keys, err := stack.rdb.Keys(context.Background(),
		sessionRedisKey("*")).Result()
	require.NoError(t, err)
	var out []*checkout.Session
	for _, key := range keys {
		if strings.HasSuffix(key, ":lock") ||
			strings.HasSuffix(key, ":order") {
			continue
		}
		out = append(out, stack.storedSession(t,
			strings.TrimPrefix(key, sessionRedisKey(""))))
	}
	return out
}

// homeDelivery is a fulfillment whose country has regions, so a create
// that carries it asks the store for them.
func homeDelivery() map[string]any {
	return map[string]any{
		"kind": "home_delivery", "providerCode": "acs",
		"countryCode": "GR", "city": "Αθήνα", "zipcode": "10431",
		"street": "Πανεπιστημίου", "streetNumber": "12",
		"regionId": "GR-14",
	}
}

const regionRoute = "GET /api/v1/region/GR/get_regions_by_country_alpha_2"

// Creates that name one cart at the same moment all find it free. The
// store lets one take it. Each of the others then cancels the checkout
// that holds the cart and takes its place, or gives up: however many
// asked, one checkout on the cart is left that is not canceled, and no
// call is given, or told of, a checkout that is not its own.
func TestConcurrentCreatesOnOneCartLeaveOneLiveCheckout(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	p := newPlatform(t, stack)
	cart := stack.faults.carts.mint()
	const n = 6
	// Each create has read the cart's holder, and found none, by the time
	// it asks the store for the regions: held there, all n go on to take
	// the cart together.
	gates := stack.hold(t, regionRoute, n)
	answers := make(chan createAnswer, n)
	for range n {
		go func() {
			answers <- p.create(ctx, cart, map[string]any{
				"fulfillment": homeDelivery(),
			})
		}()
	}
	for _, g := range gates {
		await(t, g, "a create")
	}
	for _, g := range gates {
		g.open()
	}

	all := make([]createAnswer, 0, n)
	var given []string
	for range n {
		a := <-answers
		all = append(all, a)
		if a.refusal == "" {
			given = append(given, a.id)
			continue
		}
		// It found the cart's holder changed in every round.
		assert.Contains(t, a.refusal, `"code":"conflict"`)
		assert.Contains(t, a.refusal, "this call created none")
	}
	require.NotEmpty(t, given, "no create took the cart")

	// Each stored session was given to one call, and one of them is not
	// canceled: the one the cart's record names.
	var stored, live []string
	for _, s := range stack.sessions(t) {
		require.Equal(t, cart, s.CartID)
		stored = append(stored, s.ID)
		if s.Status != checkout.StatusCanceled {
			live = append(live, s.ID)
		}
	}
	assert.ElementsMatch(t, given, stored)
	require.Len(t, live, 1, "one cart has %d live checkouts", len(live))
	assert.Equal(t, live[0], heldBy(t, stack.rdb, cart))
	// No answer carries the id of a checkout another call was given.
	for _, a := range all {
		for _, id := range stored {
			if id != a.id {
				assert.NotContains(t, a.text+a.refusal, id)
			}
		}
	}
}

// What create_checkout answers for a cart, by the checkout that holds
// it. The newest create holds the cart: an open checkout on it is
// canceled. A checkout whose order is being placed or awaits its payment
// keeps the cart, and no answer hands that checkout out or names it.
func TestCreateCheckoutOnACartInACheckout(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	p := newPlatform(t, stack)
	acpBase := stack.gw.URL + "/acp/checkout_sessions"
	// refused checks a create that the cart's holder refused: it started
	// no checkout, left the holder as it was, and named neither the
	// holder nor its order.
	refused := func(
		t *testing.T, cart, holder string, submit map[string]any,
		parts ...string,
	) {
		t.Helper()
		sessions := stack.sessionCount(t)
		stored := stack.storedBytes(t, holder)
		a := p.create(ctx, cart, submit)
		assert.Empty(t, a.id, "a second checkout was started")
		for _, part := range parts {
			assert.Contains(t, a.refusal, part)
		}
		assert.NotContains(t, a.refusal, holder)
		assert.NotContains(t, a.refusal, fixtureOrderUUID)
		assert.NotContains(t, a.refusal, "start a new")
		assert.Equal(t, sessions, stack.sessionCount(t))
		assert.Equal(t, stored, stack.storedBytes(t, holder),
			"the holder was written")
		assert.Equal(t, holder, heldBy(t, stack.rdb, cart))
	}
	// replaced checks a create that started a new checkout on the cart,
	// and returns its id.
	replaced := func(t *testing.T, cart, old string) string {
		t.Helper()
		a := p.create(ctx, cart, nil)
		require.Empty(t, a.refusal)
		assert.NotEqual(t, old, a.id)
		assert.NotContains(t, a.text, old)
		assert.Equal(t, a.id, heldBy(t, stack.rdb, cart))
		s := stack.storedSession(t, a.id)
		assert.Equal(t, cart, s.CartID)
		assert.Equal(t, checkout.StatusIncomplete, s.Status)
		return a.id
	}
	// ready is the id of a cash-on-delivery checkout on the cart that is
	// ready to complete.
	ready := func(t *testing.T, cart string) string {
		t.Helper()
		a := p.create(ctx, cart, map[string]any{
			"buyer": map[string]any{
				"first_name": "Μαρία", "last_name": "Παπαδοπούλου",
				"email": "maria@example.test", "phone_number": "+306912345678",
			},
			"fulfillment": homeDelivery(),
			"payment":     codPayment(),
		})
		require.Empty(t, a.refusal)
		require.Equal(t, checkout.StatusReadyForComplete,
			stack.storedSession(t, a.id).Status)
		return a.id
	}

	t.Run("an open checkout is canceled, and the new one holds the cart",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			old := ready(t, cart)
			orders := stack.faults.orderCreates.Load()

			replaced(t, cart, old)

			assert.Equal(t, checkout.StatusCanceled,
				stack.storedSession(t, old).Status)
			// The old id is a canceled checkout's from then on.
			res := callTool(t, p.session, "complete_checkout", map[string]any{
				"meta": p.meta(uuid.NewString()), "id": old,
				"checkout": map[string]any{"payment": codPayment()},
			})
			require.True(t, res.IsError)
			assert.Contains(t, res.Content[0].(*mcp.TextContent).Text,
				"is canceled and cannot be completed")
			assert.Equal(t, orders, stack.faults.orderCreates.Load())
		})

	// What the create submits is settled first: a create the store
	// refuses has canceled nothing.
	t.Run("a refused create leaves the open checkout alone",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			old := ready(t, cart)

			refused(t, cart, old, map[string]any{
				"payment": map[string]any{"instruments": []any{
					map[string]any{
						"handler_id": "com.example.elsewhere",
						"type":       "card",
					},
				}},
			}, "unknown payment handler_id")

			stack.faults.regionLookup.Store(true)
			defer stack.faults.regionLookup.Store(false)
			refused(t, cart, old, map[string]any{
				"fulfillment": homeDelivery(),
			}, `"code":"unavailable"`, "the store's backend")
			assert.Equal(t, checkout.StatusReadyForComplete,
				stack.storedSession(t, old).Status)
		})

	// Django reads every spelling of a UUID as one cart, so every
	// spelling names the cart its checkout holds.
	t.Run("another spelling of the cart's id is the same cart",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			holder := p.started(t, cart)
			for _, spelling := range []string{
				strings.ToUpper(cart),
				"{" + cart + "}",
				"urn:uuid:" + cart,
				strings.ReplaceAll(cart, "-", ""),
			} {
				a := p.create(ctx, spelling, nil)
				require.Empty(t, a.refusal, spelling)
				assert.Equal(t, checkout.StatusCanceled,
					stack.storedSession(t, holder).Status, spelling)
				assert.Equal(t, a.id, heldBy(t, stack.rdb, cart), spelling)
				assert.Equal(t, cart, stack.storedSession(t, a.id).CartID)
				holder = a.id
			}
			// One cart, one record, whatever it was called.
			keys, err := stack.rdb.Keys(ctx, cartRecordPrefix+"*").Result()
			require.NoError(t, err)
			hex := strings.NewReplacer("-", "", "{", "", "}", "",
				"urn:uuid:", "")
			var records []string
			for _, key := range keys {
				named := strings.TrimPrefix(key, cartRecordPrefix)
				if hex.Replace(strings.ToLower(named)) == hex.Replace(cart) {
					records = append(records, key)
				}
			}
			assert.Equal(t, []string{cartRecordPrefix + cart}, records)
		})

	t.Run("an open checkout of another protocol keeps the cart",
		func(t *testing.T) {
			acpID := acpReady(t, acpBase)
			cart := stack.storedSession(t, acpID).CartID
			refused(t, cart, acpID, nil, `"code":"conflict"`,
				"started outside this server", "no checkout was created")
		})

	for name, tc := range map[string]struct {
		become func(*checkout.Session)
		parts  []string
	}{
		"its order being placed": {func(s *checkout.Session) {
			s.Status = checkout.StatusCompleteInProgress
		}, []string{"order is being placed", "an order exists or may"}},
		"its order sent and never confirmed": {func(s *checkout.Session) {
			s.Status = checkout.StatusCompleteInProgress
			s.Unconfirmed = true
		}, []string{"never confirmed", "an order exists or may"}},
		"its order awaiting payment": {func(s *checkout.Session) {
			s.Status = checkout.StatusRequiresEscalation
			s.OrderID, s.OrderUUID = 684, fixtureOrderUUID
			s.PlacedAt = time.Now()
		}, []string{"awaits its payment", "an order exists"}},
	} {
		for _, protocol := range []string{"ucp", "acp"} {
			t.Run("a checkout with "+name+" keeps the cart, over "+protocol,
				func(t *testing.T) {
					var holder string
					if protocol == "acp" {
						holder = acpReady(t, acpBase)
					} else {
						holder = ready(t, stack.faults.carts.mint())
					}
					cart := stack.storedSession(t, holder).CartID
					stack.become(t, holder, tc.become)

					refused(t, cart, holder, map[string]any{
						"fulfillment": homeDelivery(),
						"discounts":   map[string]any{"codes": []string{"SAVE10"}},
					}, append(tc.parts, `"code":"conflict"`,
						"no checkout was created",
						"do not start another checkout",
						"the store's order confirmation")...)
				})
		}
	}

	// The refusal comes before the create asks the store anything or
	// writes the cart.
	t.Run("a cart that is kept is neither looked up for nor written",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			holder := ready(t, cart)
			stack.become(t, holder, func(s *checkout.Session) {
				s.Status = checkout.StatusCompleteInProgress
			})
			writes := stack.faults.cartWrites.Load()
			var asked atomic.Int32
			before := func(*http.Request) { asked.Add(1) }
			stack.faults.before.Store(&before)
			defer stack.faults.before.Store(nil)

			refused(t, cart, holder, map[string]any{
				"fulfillment": homeDelivery(),
				"payment":     codPayment(),
				"discounts":   map[string]any{"codes": []string{"SAVE10"}},
			}, "order is being placed")

			assert.Zero(t, asked.Load(), "the store was asked")
			assert.Equal(t, writes, stack.faults.cartWrites.Load())
		})

	// The create read an open holder, and the holder's completion marked
	// it in progress before the create's cancel reached Redis: the cancel
	// loses, and the create decides again, on what the holder is now.
	t.Run("a holder that starts placing its order is not canceled",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			holder := ready(t, cart)
			sessions := stack.sessionCount(t)
			// The hook runs on the gateway's goroutine: it reports its
			// error and asserts nothing.
			marked := make(chan error, 1)
			stack.beforeScriptOn(t, sessionRedisKey(holder), func() {
				store := checkout.NewStore(stack.rdb, ucpUpstreamTimeout)
				s, err := store.Load(ctx, "demostore", holder)
				if err == nil {
					s.Status = checkout.StatusCompleteInProgress
					err = store.Save(ctx, s)
				}
				marked <- err
			})

			a := p.create(ctx, cart, nil)

			select {
			case err := <-marked:
				require.NoError(t, err)
			default:
				t.Fatal("the create never tried to cancel the holder")
			}
			assert.Empty(t, a.id, "a second checkout was started")
			assert.Contains(t, a.refusal, "order is being placed")
			assert.NotContains(t, a.refusal, holder)
			assert.Equal(t, checkout.StatusCompleteInProgress,
				stack.storedSession(t, holder).Status)
			assert.Equal(t, holder, heldBy(t, stack.rdb, cart))
			assert.Equal(t, sessions, stack.sessionCount(t))
		})

	// Django cancels an unpaid online order when its payment window
	// ends and keeps the cart, while the checkout reads "awaiting
	// payment" for its whole day. The order is asked: a shopper who left
	// the payment page can check out the cart again.
	awaitingPayment := func(s *checkout.Session) {
		s.Status = checkout.StatusRequiresEscalation
		s.OrderID, s.OrderUUID = 684, fixtureOrderUUID
		s.PlacedAt = time.Now()
	}
	t.Run("a checkout whose unpaid order was canceled leaves the cart",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			old := ready(t, cart)
			stack.become(t, old, awaitingPayment)
			stack.faults.orderCanceled.Store(true)
			defer stack.faults.orderCanceled.Store(false)

			replaced(t, cart, old)

			assert.Equal(t, checkout.StatusCanceled,
				stack.storedSession(t, old).Status)
		})

	t.Run("an order awaiting payment that cannot be read keeps the cart",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			old := ready(t, cart)
			stack.become(t, old, awaitingPayment)

			stack.faults.orderRead.Store(true)
			refused(t, cart, old, nil, `"code":"unavailable"`,
				"No checkout was created")
			stack.faults.orderRead.Store(false)

			// An order Django no longer has is not a canceled one.
			stack.faults.orderGone.Store(true)
			defer stack.faults.orderGone.Store(false)
			refused(t, cart, old, nil, `"code":"conflict"`,
				"awaits its payment")
		})

	// A cash-on-delivery order empties its cart, and the shopper may
	// fill the same cart again.
	t.Run("a completed checkout leaves the cart to a new one",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			old := p.started(t, cart)
			stack.become(t, old, func(s *checkout.Session) {
				s.Status = checkout.StatusCompleted
				s.OrderID, s.OrderUUID = 684, fixtureOrderUUID
				s.PlacedAt = time.Now()
			})
			replaced(t, cart, old)
			assert.Equal(t, checkout.StatusCompleted,
				stack.storedSession(t, old).Status)
		})

	t.Run("a canceled checkout leaves the cart to a new one",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			old := p.started(t, cart)
			res := callTool(t, p.session, "cancel_checkout", map[string]any{
				"meta": p.meta(uuid.NewString()), "id": old,
			})
			require.False(t, res.IsError, "%v", res.Content)
			replaced(t, cart, old)
		})

	t.Run("a checkout that is gone leaves the cart to a new one",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			old := p.started(t, cart)
			stack.lapse(t, sessionRedisKey(old))
			replaced(t, cart, old)
		})
}

// A checkout started from line items, on either protocol, holds the cart
// that was made for it. Its continue_url discloses that cart's id, so a
// create_checkout naming the cart must meet the checkout.
func TestACheckoutFromLineItemsHoldsItsCart(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	p := newPlatform(t, stack)

	t.Run("ucp", func(t *testing.T) {
		a := p.create(ctx, "", map[string]any{"line_items": []any{
			map[string]any{"item": map[string]any{"id": "1"}, "quantity": 2},
		}})
		require.Empty(t, a.refusal)
		s := stack.storedSession(t, a.id)
		require.NotEqual(t, fixtureCartID, s.CartID)
		assert.Equal(t, a.id, heldBy(t, stack.rdb, s.CartID))
		assertLivesWithSession(t, stack.rdb, s, 30*time.Minute)

		// A create naming the cart finds it held, and takes it over.
		next := p.create(ctx, s.CartID, nil)
		require.Empty(t, next.refusal)
		assert.NotEqual(t, a.id, next.id)
		assert.Equal(t, checkout.StatusCanceled,
			stack.storedSession(t, a.id).Status)
		assert.Equal(t, next.id, heldBy(t, stack.rdb, s.CartID))
	})

	t.Run("acp", func(t *testing.T) {
		id := acpReady(t, stack.gw.URL+"/acp/checkout_sessions")
		s := stack.storedSession(t, id)
		assert.Equal(t, id, heldBy(t, stack.rdb, s.CartID))
		assertLivesWithSession(t, stack.rdb, s, 30*time.Minute)
	})
}

// A cart Django just made is no checkout's. If its record says
// otherwise, the create hands out no session: it could be the second one
// on a cart.
func TestACartJustMadeThatIsHeldStartsNoCheckout(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	p := newPlatform(t, stack)
	// Before a create takes its cart, another session's name is put in
	// the cart's record.
	var taken atomic.Int32
	stack.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		args := cmd.Args()
		if (cmd.Name() != "evalsha" && cmd.Name() != "eval") ||
			len(args) < 5 {
			return nil
		}
		record, ok := args[4].(string)
		if !ok || !strings.HasPrefix(record, cartRecordPrefix) {
			return nil
		}
		taken.Add(1)
		return stack.rdb.Set(ctx, record, uuid.NewString(),
			time.Minute).Err()
	}))

	t.Run("ucp", func(t *testing.T) {
		a := p.create(ctx, "", map[string]any{"line_items": []any{
			map[string]any{"item": map[string]any{"id": "1"}, "quantity": 2},
		}})
		assert.Empty(t, a.id)
		assert.Contains(t, a.refusal, `"code":"unavailable"`)
		assert.NotEmpty(t, stack.logs.with(
			"ucp: a cart just made is already in a checkout"))
	})

	t.Run("acp", func(t *testing.T) {
		resp, refusal := acpCall(t, http.MethodPost,
			stack.gw.URL+"/acp/checkout_sessions", map[string]any{
				"line_items":   []map[string]any{{"id": "1", "quantity": 2}},
				"currency":     "EUR",
				"capabilities": map[string]any{},
			}, uuid.NewString())
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, "unavailable", refusal["code"])
		assert.NotEmpty(t, stack.logs.with(
			"acp: a cart just made is already in a checkout"))
	})

	assert.Positive(t, taken.Load())
	assert.Zero(t, stack.sessionCount(t))
}

// Django's cart is outside the session's compare-and-set, so a cart is
// written only by the checkout that holds it, in that checkout's turn.
// A create writes its discount codes once its checkout has taken the
// cart, and cancels the checkout that held it only in that checkout's
// turn: under its lock.
func TestACreateWritesItsCartOnlyOnceItHoldsIt(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	stack.faults.promotions.Store(true)
	p := newPlatform(t, stack)
	store := checkout.NewStore(stack.rdb, ucpUpstreamTimeout)
	codes := map[string]any{
		"discounts": map[string]any{"codes": []string{"SAVE10"}},
	}

	t.Run("the code is written when the new checkout holds the cart",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			old := p.started(t, cart)
			// Who held the cart when the coupon reached the store. The
			// hook runs on the fake's goroutine and asserts nothing.
			var holder atomic.Pointer[string]
			before := func(r *http.Request) {
				if r.Method == http.MethodPost &&
					r.URL.Path == "/api/v1/cart/coupon" {
					id, _ := stack.rdb.Get(ctx, cartRecordPrefix+cart).Result()
					holder.Store(&id)
				}
			}
			stack.faults.before.Store(&before)
			defer stack.faults.before.Store(nil)

			a := p.create(ctx, cart, codes)

			require.Empty(t, a.refusal)
			require.NotNil(t, holder.Load(), "no coupon was written")
			assert.Equal(t, a.id, *holder.Load(),
				"the coupon was written before its checkout held the cart")
			assert.Equal(t, []string{"SAVE10"},
				stack.storedSession(t, a.id).DiscountCodes)
			assert.Equal(t, checkout.StatusCanceled,
				stack.storedSession(t, old).Status)
		})

	t.Run("a code write that fails leaves no checkout", func(t *testing.T) {
		cart := stack.faults.carts.mint()
		old := p.started(t, cart)
		sessions := stack.sessionCount(t)
		stack.faults.couponWrite.Store(true)
		defer stack.faults.couponWrite.Store(false)

		a := p.create(ctx, cart, codes)

		assert.Empty(t, a.id)
		assert.Contains(t, a.refusal, "No checkout was created")
		assert.Equal(t, sessions, stack.sessionCount(t),
			"the unanswered checkout was kept")
		assert.Empty(t, heldBy(t, stack.rdb, cart))
		assert.Equal(t, checkout.StatusCanceled,
			stack.storedSession(t, old).Status)
	})

	// Another create takes the cart between this one's read of its
	// holder and its own take, and starts placing its order.
	t.Run("a create that loses the cart writes nothing to it",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			winner := sessionOn(cart)
			var armed atomic.Bool
			armed.Store(true)
			took := make(chan error, 1)
			stack.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
				args := cmd.Args()
				if (cmd.Name() != "evalsha" && cmd.Name() != "eval") ||
					len(args) < 5 || args[4] != cartRecordPrefix+cart ||
					!armed.CompareAndSwap(true, false) {
					return nil
				}
				err := store.Create(ctx, winner, checkout.CartHolder{})
				if err == nil {
					winner.Status = checkout.StatusCompleteInProgress
					err = store.Save(ctx, winner)
				}
				took <- err
				return nil
			}))
			t.Cleanup(func() { armed.Store(false) })
			writes := stack.faults.cartWrites.Load()

			a := p.create(ctx, cart, codes)

			select {
			case err := <-took:
				require.NoError(t, err)
			default:
				t.Fatal("the create never tried to take the cart")
			}
			assert.Empty(t, a.id, "a second checkout was started")
			assert.Contains(t, a.refusal, "order is being placed")
			assert.NotContains(t, a.refusal, winner.ID)
			assert.Equal(t, writes, stack.faults.cartWrites.Load(),
				"the cart of another checkout was written")
			assert.Equal(t, winner.ID, heldBy(t, stack.rdb, cart))
		})

	// A call at work on the holder (an update writing the cart, a
	// completion) holds its lock.
	t.Run("a checkout another call is at work on is not canceled",
		func(t *testing.T) {
			cart := stack.faults.carts.mint()
			holder := p.started(t, cart)
			stored := stack.storedBytes(t, holder)
			sessions := stack.sessionCount(t)
			writes := stack.faults.cartWrites.Load()
			release, err := store.Lock(ctx, "demostore", holder)
			require.NoError(t, err)
			defer release()

			a := p.create(ctx, cart, codes)

			assert.Empty(t, a.id, "a second checkout was started")
			assert.Contains(t, a.refusal, `"code":"conflict"`)
			assert.Contains(t, a.refusal, "another request is at work")
			assert.NotContains(t, a.refusal, holder)
			assert.Equal(t, stored, stack.storedBytes(t, holder),
				"the holder was written")
			assert.Equal(t, holder, heldBy(t, stack.rdb, cart))
			assert.Equal(t, sessions, stack.sessionCount(t))
			assert.Equal(t, writes, stack.faults.cartWrites.Load())
		})
}
