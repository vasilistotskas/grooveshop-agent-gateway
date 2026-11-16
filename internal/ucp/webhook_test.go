package ucp

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A delivery's target is the party its endpoint belongs to: hosts under
// one registrable domain share a target, whatever their port, scheme,
// path or spelling, so subdomains cost nothing extra a share of the
// workers; a hosting domain on the public suffix list's private section
// is one target per customer; an address is itself, an IPv6 one its /64.
func TestDeliveryTarget(t *testing.T) {
	for raw, want := range map[string]string{
		"https://hooks.platform.example/ucp/orders":      "platform.example",
		"https://A.B.Platform.Example./x?token=1":        "platform.example",
		"https://platform.example:8443/orders":           "platform.example",
		"http://platform.example/orders":                 "platform.example",
		"https://orders.shop.co.uk/hook":                 "shop.co.uk",
		"https://one.github.io/hook":                     "one.github.io",
		"https://two.github.io/hook":                     "two.github.io",
		"https://203.0.113.9:8443/hook":                  "203.0.113.9",
		"https://[::ffff:203.0.113.9]/hook":              "203.0.113.9",
		"https://[2001:db8:1:2:3:4:5:6]/hook":            "2001:db8:1:2::/64",
		"https://[2001:db8:1:2:ffff::1]/hook":            "2001:db8:1:2::/64",
		"http://localhost:8080/hook":                     "localhost",
		"http://127.0.0.1:8080/hook":                     "127.0.0.1",
		"https://user@platform.example/hook":             "platform.example",
		"https://platform.example/../../other.example/x": "platform.example",
		"https://hooks.bücher.example/x":                 "xn--bcher-kva.example",
		"https://hooks.BÜCHER.example/x":                 "xn--bcher-kva.example",
		"https://hooks.xn--bcher-kva.example/x":          "xn--bcher-kva.example",
		"https://a.ελληνικά.gr/x":                        "xn--hxargifdar.gr",
	} {
		assert.Equal(t, want, deliveryTarget(raw), raw)
	}
	assert.NotEqual(t, deliveryTarget("https://a.one.example/x"),
		deliveryTarget("https://a.two.example/x"))
}

// testJob is a delivery to target read at readAt.
func testJob(id, target string, readAt time.Time) job {
	return job{id: id, target: target, readAt: readAt}
}

// A target holds at most its cap of slots; what is read beyond it waits
// in the target's queue, in order, and the worker that finishes one of
// the target's deliveries takes the next on the same slot. Other targets
// are not held up.
func TestTargetSlotsQueueBehindTheCap(t *testing.T) {
	now := time.Now()
	slots := newTargetSlots(2, 3, 10)
	assert.Equal(t, admitted, slots.admit(testJob("1", "a", now)))
	assert.Equal(t, admitted, slots.admit(testJob("2", "a", now)))
	assert.Equal(t, queued, slots.admit(testJob("3", "a", now)))
	assert.Equal(t, queued, slots.admit(testJob("4", "a", now)))
	assert.Equal(t, admitted, slots.admit(testJob("5", "b", now)),
		"another target is not held up")

	j, stale, ok := slots.next("a", now, true)
	require.True(t, ok)
	assert.Empty(t, stale)
	assert.Equal(t, "3", j.id, "the queue is first in, first out")
	j, _, ok = slots.next("a", now, true)
	require.True(t, ok)
	assert.Equal(t, "4", j.id)
	assert.Equal(t, 2, slots.held["a"], "a queued delivery keeps the slot")

	_, _, ok = slots.next("a", now, true)
	assert.False(t, ok)
	_, _, ok = slots.next("a", now, true)
	assert.False(t, ok)
	_, _, ok = slots.next("b", now, true)
	assert.False(t, ok)
	assert.Empty(t, slots.held, "a target with nothing in flight is forgotten")
	assert.Empty(t, slots.queue)
	assert.Zero(t, slots.queued)
}

// The queues are bounded per target and in all; beyond either, a
// delivery is refused, to be left pending for reclaim.
func TestTargetSlotsBoundTheQueues(t *testing.T) {
	now := time.Now()
	slots := newTargetSlots(1, 2, 3)
	for _, target := range []string{"a", "b"} {
		assert.Equal(t, admitted, slots.admit(testJob("x", target, now)))
	}
	assert.Equal(t, queued, slots.admit(testJob("a1", "a", now)))
	assert.Equal(t, queued, slots.admit(testJob("a2", "a", now)))
	assert.Equal(t, refused, slots.admit(testJob("a3", "a", now)),
		"a target's queue is bounded")
	assert.Equal(t, queued, slots.admit(testJob("b1", "b", now)))
	assert.Equal(t, refused, slots.admit(testJob("b2", "b", now)),
		"all queues together are bounded")
}

// A queued delivery that waited past the bound is dropped as stale, so a
// pod never holds an entry long enough for reclaim to take it over; a
// stopped consumer takes nothing more from the queue.
func TestTargetSlotsDropWhatWaitedTooLong(t *testing.T) {
	now := time.Now()
	slots := newTargetSlots(1, 5, 5)
	slots.admit(testJob("1", "a", now))
	slots.admit(testJob("old", "a", now.Add(-2*time.Minute)))
	slots.admit(testJob("fresh", "a", now))

	j, stale, ok := slots.next("a", now, true)
	require.True(t, ok)
	assert.Equal(t, "fresh", j.id)
	require.Len(t, stale, 1)
	assert.Equal(t, "old", stale[0].id)

	slots.admit(testJob("later", "a", now))
	_, _, ok = slots.next("a", now, false)
	assert.False(t, ok, "a stopped consumer starts nothing queued")
	assert.Empty(t, slots.held)
}

// Reclaim takes over an entry once it has been idle visibilityTimeout:
// that must outlast everything a live pod does with an entry it read
// (start within queueMaxWait, finish within deliveryBudget, acknowledge
// within ackTimeout), or the entry is sent twice.
func TestVisibilityTimeoutOutlastsWhatAPodHolds(t *testing.T) {
	assert.Greater(t, visibilityTimeout,
		queueMaxWait+deliveryBudget+ackTimeout)
}

// testDispatcher is a Dispatcher for handOut alone: no Redis.
func testDispatcher() *Dispatcher {
	return &Dispatcher{log: slog.New(slog.DiscardHandler)}
}

// With every worker busy, a delivery is not held past the bound: the
// send gives up at its read time plus maxWait, frees the target's slot
// and leaves the entry pending, and the rest of the batch, read at the
// same moment, is past the bound too and left pending at once.
func TestHandOutHoldsNothingPastTheBoundWhenEveryWorkerIsBusy(t *testing.T) {
	d := testDispatcher()
	slots := newTargetSlots(2, 64, 256)
	busy := make(chan job) // no worker takes anything
	readAt := time.Now()
	batch := []job{
		testJob("1", "a", readAt), testJob("2", "b", readAt),
		testJob("3", "c", readAt),
	}
	const maxWait = 150 * time.Millisecond

	start := time.Now()
	require.True(t, d.handOut(t.Context(), busy, slots, batch,
		maxWait))
	took := time.Since(start)
	assert.GreaterOrEqual(t, took, maxWait-10*time.Millisecond)
	assert.Less(t, took, maxWait+time.Second,
		"one bound for the batch, not one per job")
	assert.Empty(t, slots.held, "every slot is freed")
	assert.Empty(t, slots.queue)
}

// A job read longer ago than the bound is neither handed out nor queued.
func TestHandOutLeavesAStaleJobPending(t *testing.T) {
	d := testDispatcher()
	slots := newTargetSlots(1, 64, 256)
	// Its target is at its cap, so it would be queued.
	require.Equal(t, admitted, slots.admit(testJob("busy", "a", time.Now())))
	jobs := make(chan job, 1)
	stale := testJob("old", "a", time.Now().Add(-time.Minute))
	require.True(t, d.handOut(t.Context(), jobs, slots,
		[]job{stale}, time.Second))
	assert.Empty(t, jobs)
	assert.Empty(t, slots.queue, "a stale job is not queued")
	assert.Equal(t, 1, slots.held["a"])
}

// A free worker gets the job at once; one beyond the target's cap is
// queued, not sent.
func TestHandOutGivesAFreeWorkerTheJob(t *testing.T) {
	d := testDispatcher()
	slots := newTargetSlots(1, 64, 256)
	jobs := make(chan job, 2)
	now := time.Now()
	require.True(t, d.handOut(t.Context(), jobs, slots,
		[]job{testJob("1", "a", now), testJob("2", "a", now)}, time.Second))
	require.Len(t, jobs, 1)
	assert.Equal(t, "1", (<-jobs).id)
	assert.Len(t, slots.queue["a"], 1)
}

// A target whose last slot is freed while a delivery is queued behind it
// (the hand-off of the slot's job gave up) has its queue dropped: nothing
// of the target is in flight to take the next one, and a free slot must
// mean an empty queue. The dropped delivery is left pending.
func TestReleasingATargetsLastSlotDropsItsQueue(t *testing.T) {
	now := time.Now()
	slots := newTargetSlots(1, 64, 256)
	require.Equal(t, admitted, slots.admit(testJob("A", "t", now)))
	require.Equal(t, queued, slots.admit(testJob("B", "t", now)))
	require.Equal(t, admitted, slots.admit(testJob("C", "u", now)))
	require.Equal(t, queued, slots.admit(testJob("D", "u", now)))

	dropped := slots.release("t")
	require.Len(t, dropped, 1)
	assert.Equal(t, "B", dropped[0].id)
	assert.NotContains(t, slots.held, "t")
	assert.NotContains(t, slots.queue, "t")
	assert.Equal(t, 1, slots.queued, "only the other target's queue is left")
	assert.Equal(t, admitted, slots.admit(testJob("E", "t", now)),
		"a free slot takes the next delivery at once")

	// A stopped consumer's worker frees the slot the same way.
	_, dropped, ok := slots.next("u", now, false)
	assert.False(t, ok)
	require.Len(t, dropped, 1)
	assert.Equal(t, "D", dropped[0].id)
	assert.Zero(t, slots.queued)
	assert.Empty(t, slots.queue)
}

// A stopped consumer hands nothing out and frees the slot it took.
func TestHandOutStopsWithItsContext(t *testing.T) {
	d := testDispatcher()
	slots := newTargetSlots(2, 64, 256)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.False(t, d.handOut(ctx, make(chan job), slots,
		[]job{testJob("1", "a", time.Now())}, time.Minute))
	assert.Empty(t, slots.held)
}
