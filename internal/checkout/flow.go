package checkout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// ErrNotReady marks a complete call on a session that still misses data.
var ErrNotReady = errors.New("checkout: session is not ready to complete")

// ErrChoiceRefused marks a complete call on a session the store refuses
// a choice or the data of now (Pricing.Refused): it is ErrNotReady too.
var ErrChoiceRefused = fmt.Errorf(
	"%w: the store refuses one of its choices", ErrNotReady)

// ErrTotalNotShown marks a complete call on a session no stored render
// showed a total for: its order would have no total to be placed at. It
// is ErrNotReady too; reading the checkout stores one.
var ErrTotalNotShown = fmt.Errorf(
	"%w: it has shown no total yet", ErrNotReady)

// ErrPaymentMethodUnsupported marks a pay way the agentic surface cannot
// finish: any online pay way other than Viva hosted authorization (Stripe
// included), or one whose settlement is unknown.
var ErrPaymentMethodUnsupported = errors.New(
	"checkout: this payment method cannot be completed by an agent yet")

// ErrOrderOutcomeUnknown marks an order creation whose result never
// arrived, after every attempt (OrderRetry): a timeout, a dropped
// connection, a 5xx after the commit, an unreadable success. The order
// may exist. The session stays complete_in_progress, stored unconfirmed,
// and the next complete resumes it (Resume): Django answers the order
// with the checkout's id the order it placed, if it placed one, so asking
// again never orders twice. A Resume that cannot ask is this error too.
var ErrOrderOutcomeUnknown = errors.New(
	"checkout: the store did not confirm whether the order was placed")

// ErrPaymentSessionFailed marks an order that exists but whose hosted
// payment link could not be created. The session stays escalated and
// ResumePayment mints a fresh link; callers must never report it as a
// failed order — the buyer would check out again and order twice.
var ErrPaymentSessionFailed = errors.New(
	"checkout: the order is placed but its payment link could not be " +
		"created")

// ErrPaymentWindowClosed marks an order that can no longer be paid: its
// payment window has run out, or it is paid or no longer open for
// payment (Django's payment_window_expired, order_not_payable). It is
// final for that order: no link is minted for it again.
var ErrPaymentWindowClosed = errors.New(
	"checkout: the order can no longer be paid")

// ErrPaymentAwaitsStore marks an order whose payment no new link can
// help: the provider is processing one, a charge that did not match its
// total waits for the store (paymentAwaitsReview), or Django holds no
// payment open for it. The checkout says which, and no link is minted.
var ErrPaymentAwaitsStore = errors.New(
	"checkout: the order's payment awaits the store or its provider")

// ErrSessionNotSaved marks an order that is placed while its session
// could not be stored: the stored session still reads
// complete_in_progress.
var ErrSessionNotSaved = errors.New(
	"checkout: the order is placed but its session could not be saved")

// beforeTheView reports whether Django refused a request before its view
// ran: DRF throttles, authenticates and checks permissions first
// (APIView.initial), so a 429, 401 or 403 to an order creation never
// reached the lookup that answers a repeat with the checkout's order.
func beforeTheView(err error) bool {
	return errors.Is(err, django.ErrThrottled) ||
		errors.Is(err, django.ErrUnauthorized) ||
		errors.Is(err, django.ErrForbidden)
}

// refused reports whether Django definitively rejected a request (a 4xx),
// so nothing was created and it is safe to try again.
func refused(err error) bool {
	for _, sentinel := range []error{
		django.ErrValidation, django.ErrConflict, django.ErrNotFound,
		django.ErrThrottled, django.ErrUnauthorized, django.ErrForbidden,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// Placement is what a completion did about the order, whatever error
// came with it. Each surface settles its idempotency key by it.
type Placement int

const (
	// OrderMayExist: an order exists, or may. The caller spends its key,
	// so a retry re-renders the session and never places a second order.
	// It is the zero value: a Placement nobody set must not release a
	// key.
	OrderMayExist Placement = iota
	// NothingPlaced: refused, and nothing was created. UCP releases its
	// completion claim, so the same key can try again; ACP's key keeps
	// a refusal's answer like any other (acp.keptAgainstKey).
	NothingPlaced
)

// completionSteps sizes the part of Complete that runs without its
// caller, in upstream timeouts: one for the order, one for its payment
// session, one for the Redis writes around them. The Django client cuts
// each of its calls at one upstream timeout by itself, so however slow
// Django answers, the writes keep their share.
const completionSteps = 3

// CompletionBudget is the longest the part of Complete that runs without
// its caller can take, for a Django client cut at upstreamTimeout: what
// a shutdown has to wait for once it stops waiting for callers.
func CompletionBudget(upstreamTimeout time.Duration) time.Duration {
	return completionSteps * upstreamTimeout
}

// settleSteps counts what a completion's request does on its own once
// Complete has returned, each step on a context of its own from
// Store.Settling: settle its idempotency key (UCP's by the placement,
// ACP's by the answer it sent), then release the session lock
// (Store.Lock). A step added there is counted
// here, or the shutdown window does not hold it.
const settleSteps = 2

// CompletionTail is the longest an order completion keeps its request
// once the request's context has ended, for a Django client cut at
// upstreamTimeout: the part of Complete that runs without its caller and
// the settle steps after it. Every Redis command in it ends at its
// context's deadline (config.RedisOptions).
func CompletionTail(upstreamTimeout time.Duration) time.Duration {
	return CompletionBudget(upstreamTimeout) +
		settleSteps*settleTimeout(upstreamTimeout)
}

// settleTimeout bounds one settle step: one Redis write, which go-redis
// alone would retry against a silent Redis for several times that.
func settleTimeout(upstreamTimeout time.Duration) time.Duration {
	return upstreamTimeout
}

// OrderRetry is how a completion sends its order again while Django
// leaves the outcome unknown (ORDER_CREATE_ATTEMPTS,
// ORDER_CREATE_RETRY_DELAY): Attempts sends in all, Delay apart. Every
// send names the checkout (django.OrderCreate.AgentCheckout), and Django
// answers a repeat with the order the checkout placed, so a repeat never
// orders twice. The attempts share the order's one upstream timeout of
// the completion's budget (completionSteps).
type OrderRetry struct {
	Attempts int
	Delay    time.Duration
}

// Flow orchestrates order creation over the existing Django endpoints.
type Flow struct {
	dj     *django.Client
	st     *Store
	site   storefront.Site
	retry  OrderRetry
	budget time.Duration
	log    *slog.Logger
}

func NewFlow(
	dj *django.Client, st *Store, site storefront.Site, retry OrderRetry,
	log *slog.Logger,
) *Flow {
	return &Flow{
		dj: dj, st: st, site: site, retry: retry,
		budget: CompletionBudget(dj.Timeout()), log: log,
	}
}

// Complete places the order for a ready session and stores the session
// as that left it. The caller settles its idempotency key by the
// returned Placement.
//
// One completion places a session's order, whatever became of the
// caller's lock and key meanwhile (both expire while a slow Django is
// still being asked): the save that marks the session in progress is
// refused unless the stored session is still the one s was loaded as,
// and nothing is sent to Django before it is stored. A completion that
// loses it gets ErrSessionChanged and has placed nothing.
//
// Once the order is about to be created, the end of the caller's context
// (a platform that hangs up, or the server cancelling its requests at
// shutdown) no longer stops anything: stopping there would leave Django
// with an order, its checkout never saved as placed and no order link
// for get_order or the order webhooks to find it by. The process can
// still end it: a graceful shutdown waits for it (CompletionTail), but
// a pod killed or lost mid-placement leaves the session in progress,
// which the next complete resumes once no completion can be running
// (Resume; docs/agent-protocols-runbook.md, section 11).
func (f *Flow) Complete(
	ctx context.Context, t *tenant.Tenant, s *Session,
) (Placement, error) {
	if err := f.admit(ctx, t, s); err != nil {
		// Nothing is stored for a session that was not admitted. A
		// snapshot that reads in progress is another attempt's, and
		// saving it would overwrite that attempt's session.
		return NothingPlaced, err
	}
	return f.run(ctx, t, s, false)
}

// Resumable reports whether a complete of s resumes its placement
// (Resume): s is in progress, and no completion of it can still be
// running (CompletionDeadline). Its order was sent and never answered,
// or its completion ended before it could store what happened.
func (f *Flow) Resumable(s *Session) bool {
	return s.Status == StatusCompleteInProgress &&
		!time.Now().Before(f.CompletionDeadline(s))
}

// Resume finishes the placement of a Resumable session, as Complete
// does from the in-progress save on: it sends the order again with the
// checkout's id, and Django answers the order the checkout placed, if
// it placed one, or places it now, or refuses it. Nothing is judged or
// read before that, since the order may exist: the session was
// admitted at the total it showed, which the order is placed at, and
// the order Django answers says how it is paid (place).
//
// Of the calls that load one resumable session, the in-progress save
// lets a single one through, as for Complete.
func (f *Flow) Resume(
	ctx context.Context, t *tenant.Tenant, s *Session,
) (Placement, error) {
	if !f.Resumable(s) {
		return NothingPlaced, ErrCompletionInProgress
	}
	return f.run(ctx, t, s, true)
}

// run places the order of a session Complete admitted, or Resume
// resumes (resumed: an order may have been sent before), and stores the
// session as that left it.
func (f *Flow) run(
	ctx context.Context, t *tenant.Tenant, s *Session, resumed bool,
) (Placement, error) {
	// No order is sent yet, so a call whose context has already ended
	// (a caller that left, a server shutting down) sends none.
	if err := ctx.Err(); err != nil {
		return NothingPlaced, err
	}

	// From here on the caller's cancellation no longer applies.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), f.budget)
	defer cancel()

	// Persisted BEFORE the order is sent, and only over the session this
	// call loaded: of the completions that read one ready session, or
	// one to resume, Redis lets a single one through here. A later
	// complete then loads a completion in flight, not a session to place
	// a second time. A resumed one is no longer unconfirmed: a
	// completion is running again.
	status, unconfirmed := s.Status, s.Unconfirmed
	s.Status, s.Unconfirmed = StatusCompleteInProgress, false
	fence := newWrite()
	err := f.st.put(ctx, s, s.Revision, fence)
	if err != nil && !errors.Is(err, ErrSessionChanged) {
		// Redis failed it, and the write may still have been stored with
		// only its reply lost: the session would then read as being
		// placed while no order was sent. One more attempt sends the same
		// write, so a first that was stored is recognised as this call's
		// and the order is placed; one that was not is written now, over
		// the session this call loaded only.
		err = f.st.put(ctx, s, s.Revision, fence)
	}
	if err != nil && !errors.Is(err, ErrSessionChanged) {
		// Both replies can be lost with the write stored. Left so, the
		// session would read as being placed, and then as an order whose
		// outcome was never recorded, until it expired, with no order
		// sent. The stored revision is this write's alone, so finding it
		// is the reply: the order is placed, as after either attempt.
		err = f.st.wasStored(ctx, s, fence, err)
	}
	if err != nil {
		// The write that guards the order was refused, by Redis failing
		// or by a session that has moved on (ErrSessionChanged): no
		// order is sent, and the session is not written again.
		s.Status, s.Unconfirmed = status, unconfirmed
		return NothingPlaced, err
	}
	placement, err := f.place(ctx, t, s, resumed)
	// Stored as the order call left it: unconfirmed (still in progress,
	// and kept as long as a placed one, since an order may exist),
	// placed, or ready again after Django refused the order.
	outcome := newWrite()
	saveErr := f.st.put(ctx, s, s.Revision, outcome)
	if saveErr != nil && !errors.Is(saveErr, ErrSessionChanged) {
		// Redis failed, and the session still reads as only marked in
		// progress, which says neither what happened nor how long to
		// keep it: one more attempt. It sends the same write, so a
		// first one that was stored and only lost its reply is
		// recognised, not taken for another writer's. If it fails too,
		// the session ends with an open one's lifetime (Session.ttl).
		// That is right for an order Django refused, which must not read
		// "an order may exist" for a day, and it is the price for the
		// other two: an order placed or unanswered is then answered from
		// the stored session for that long only, and as a missing one
		// after.
		saveErr = f.st.put(ctx, s, s.Revision, outcome)
	}
	if errors.Is(saveErr, ErrSessionChanged) && placement == OrderMayExist {
		var adopted bool
		adopted, saveErr = f.saveOverEvent(ctx, s)
		if adopted && s.Status != StatusCompleteInProgress {
			// The order this completion left unanswered is placed, and
			// the stored session says what became of it (a completion
			// that resumed this one stored it, or an event since): that
			// is the answer, not "unknown". One still in progress is the
			// resumed completion's to finish, and its outcome is not
			// stored yet: still unknown here.
			err = nil
		}
	}
	if saveErr != nil {
		f.log.ErrorContext(ctx, "checkout: session save failed",
			slog.String("session", s.ID),
			slog.String("order", s.OrderUUID),
			slog.String("error", saveErr.Error()))
		if err == nil {
			// The cause is named, not wrapped: a save lost to another
			// writer is ErrSessionChanged, which tells a caller that
			// nothing happened and to send the request again. Here the
			// order exists.
			err = fmt.Errorf("%w: %s", ErrSessionNotSaved, saveErr.Error())
		}
	}
	return placement, err
}

// saveOverEvent stores a completion's outcome over a session that was
// stored since the completion marked it in progress. Every handler
// refuses a session in progress until it is Resumable, so that other
// writer is an order event (ApplyOrderEvent), which records the order
// this completion placed and may complete the session when the payment
// has landed, a read that stored what that order said
// (Session.FollowOrder), which may too, or a completion that resumed
// this one. The completion knows more (the payment page it opened), so
// its outcome is stored over the event's, keeping "completed" if the
// event said the order is paid.
//
// A completion that knows no order (Django left it unanswered) knows
// less than a stored session that names one: an event recorded it, or a
// completion that resumed this one placed it (a reader whose clock runs
// ahead resumes early, CompletionDeadline). That session is kept as it
// is, s becomes it, and adopted reports it.
func (f *Flow) saveOverEvent(
	ctx context.Context, s *Session,
) (adopted bool, err error) {
	stored, err := f.st.Load(ctx, s.Schema, s.creator(), s.ID)
	if err != nil {
		return false, err
	}
	if s.OrderUUID == "" && stored.OrderUUID != "" {
		*s = *stored
		return true, nil
	}
	// The event's save published an expiry too: never an earlier one.
	s.ExpiresAt = later(s.ExpiresAt, stored.ExpiresAt)
	switch stored.Status {
	case StatusCompleted:
		s.Status = StatusCompleted
	case StatusCompleteInProgress:
	default:
		// Not what an event leaves: nothing is written over it.
		return false, ErrSessionChanged
	}
	s.Revision = stored.Revision
	return false, f.st.Save(ctx, s)
}

// CompletionDeadline is the latest a completion of s can still run, for a
// session stored in progress: its budget starts just before that save,
// which set s.UpdatedAt. A session still in progress after it has no
// recorded outcome, and the next complete resumes it (Resumable). The
// writer's clock sets it and the reader's judges it. A reader ahead
// resumes early, while the completion may still be running: that sends
// the order a second time, which Django answers with the checkout's one
// order (its unique index on the checkout id), and the two completions'
// saves are ordered by the session's compare-and-set (saveOverEvent
// keeps a placed outcome). The session lock keeps that rare. A writer
// ahead stamps a save in the reader's future; no completion runs longer
// than its budget from now, so the deadline is never further away than
// that.
//
// A session stored unconfirmed was stored by its completion as it ended,
// once Django left the order unanswered: none is running, so its
// deadline is that save, and the next complete resumes it at once.
func (f *Flow) CompletionDeadline(s *Session) time.Time {
	saved := s.UpdatedAt
	if now := time.Now(); saved.After(now) {
		saved = now
	}
	if s.Unconfirmed {
		return saved
	}
	return saved.Add(f.budget)
}

// admit checks that the session can be placed and reserves its stock.
// No order exists yet, so it runs on the caller's context.
func (f *Flow) admit(
	ctx context.Context, t *tenant.Tenant, s *Session,
) error {
	switch s.Status {
	case StatusReadyForComplete:
	case StatusCompleteInProgress:
		return ErrCompletionInProgress
	default:
		return fmt.Errorf("%w: missing %v", ErrNotReady, s.Missing())
	}
	// The order is placed at the total the checkout showed, from the
	// cart it was quoted for: a checkout whose render was never stored
	// has shown none, and one stored before its delivery and pay way were
	// both priced has shown no order's.
	if !s.Shown.OfAnOrder() {
		return ErrTotalNotShown
	}
	// A choice the store refuses now (the cart outgrew the carrier, the
	// pay way is no longer offered) leaves the shown total without its
	// row: the checkout is not ready, whatever its stored status says.
	pricing, _, err := ComputePricing(ctx, f.dj, t, s)
	if err != nil {
		return err
	}
	if r := pricing.Refused(); r != nil {
		return fmt.Errorf("%w: %s (%s)", ErrChoiceRefused, r.Detail,
			r.Code)
	}

	// Checked against the FINAL delivery: a method selected before the
	// carrier was chosen may be one that carrier cannot settle.
	payWay, err := availablePayWay(ctx, f.dj, t, s)
	if err != nil {
		return err
	}
	// Viva's hosted authorization and collect-later pay ways are
	// supported; every other online pay way (Stripe included) is refused,
	// since the gateway implements no tokenized card completion.
	//
	// The unknown-settlement guard comes FIRST and refuses outright.
	// This branch decides whether money is taken now or on delivery, so
	// "I could not tell" must never resolve to "collect it later" — the
	// bool this replaced would have done exactly that the day Django
	// drops the deprecated column, placing card orders as if they were
	// cash on delivery.
	if !payWay.HasKnownSettlement() {
		return fmt.Errorf(
			"%w: pay way %d reports settlement %q",
			ErrPaymentMethodUnsupported, payWay.ID, payWay.Settlement,
		)
	}
	if payWay.IsOnlineSettlement() {
		// A surface whose platform does not choose the pay way (ACP)
		// completes only with a collect-later one. Its handler chose one,
		// and the store's answer read here is what the order is placed
		// with, so the rule is checked again on that answer.
		if !s.payWaySelectionRequired() {
			return fmt.Errorf(
				"%w: pay way %d is settled online, and this surface "+
					"completes only with a collect-later one",
				ErrPaymentMethodUnsupported, payWay.ID)
		}
		if payWay.ProviderCode != django.ProviderVivaWallet {
			return ErrPaymentMethodUnsupported
		}
		// The hosted-payment gate is read at selection AND here: it can
		// be switched off in between, and a gated method is refused,
		// never honoured from a stale selection.
		if !t.HostedPaymentOn() {
			return fmt.Errorf(
				"%w: hosted payment is disabled for this store",
				ErrPaymentMethodUnsupported)
		}
	}

	if _, err := f.dj.ReserveStock(
		ctx, t.Domain, t.DefaultLocale, s.CartID,
	); err != nil {
		return err
	}
	return nil
}

// place creates the order for a session stored in progress, links it,
// and starts its payment when the order is paid online. It leaves the
// session as the result made it; run stores it. resumed: an order of
// this checkout may have been sent before (Resume).
//
// How the order is paid is the order's own word (isOnlinePayment):
// Resume reads no pay way before it sends, since staff may have
// switched it off after the order was placed. An order Django answers
// to a repeated send may be one placed earlier, whose payment may
// have moved on: it is followed as it stands (follow), never handed a
// payment link blindly.
func (f *Flow) place(
	ctx context.Context, t *tenant.Tenant, s *Session, resumed bool,
) (Placement, error) {
	order, repeated, err := f.createOrder(ctx, t, s, django.OrderCreate{
		PayWayID:             s.PayWayID,
		FirstName:            s.Buyer.FirstName,
		LastName:             s.Buyer.LastName,
		Email:                s.Buyer.Email,
		Phone:                s.Buyer.Phone,
		Street:               s.Fulfillment.Street,
		StreetNumber:         s.Fulfillment.StreetNumber,
		City:                 s.Fulfillment.City,
		Zipcode:              s.Fulfillment.Zipcode,
		CountryID:            s.Fulfillment.CountryCode,
		RegionID:             s.Fulfillment.RegionID,
		ShippingProviderCode: s.Fulfillment.ProviderCode,
		ShippingKind:         s.Fulfillment.Kind,
		PickupPoint:          s.Fulfillment.PickupPoint(),
		// Every UCP and ACP checkout response links the store's terms
		// and privacy policy for the platform to present to the buyer
		// (storefront.Terms, storefront.Privacy); completing it is the
		// buyer's acceptance. Django records it as an agent checkout,
		// proven by the internal secret.
		TermsAccepted: true,
		Attribution:   agentAttribution(s.Protocol),
		// Django creates the order only at the total the buyer was
		// shown, and answers another with django.ErrTotalChanged; only
		// from the cart that total was quoted for, and answers another
		// with django.ErrCartChanged.
		ExpectedTotal:      s.Shown.Total,
		ExpectedCartDigest: s.Shown.CartDigest,
		// One order per checkout: a repeat is answered with it.
		AgentCheckout: s.ID,
	}, resumed)
	if errors.Is(err, ErrOrderOutcomeUnknown) {
		s.Unconfirmed = true
		return OrderMayExist, err
	}
	if err != nil {
		s.Status = StatusReadyForComplete
		return NothingPlaced, err
	}
	if order.AgentCheckout != s.ID {
		// Django took the order without the checkout's id (it did not
		// believe the internal secret): a repeat of this checkout's
		// order would place another. The order is this one's all the
		// same.
		f.log.ErrorContext(ctx,
			"checkout: the store did not tie the order to its checkout",
			slog.String("session", s.ID),
			slog.String("order", order.UUID),
			slog.String("agent_checkout", order.AgentCheckout))
	}
	s.OrderID = order.ID
	s.OrderUUID = order.UUID
	// A repeat may answer an order placed long before: the session's day
	// counts from the order's creation. One whose time cannot be read is
	// kept a day from this save (Session.keptUntil).
	if placed, err := order.Created(); err != nil {
		f.log.ErrorContext(ctx, "checkout: order creation time unreadable",
			slog.String("session", s.ID),
			slog.String("order", order.UUID),
			slog.String("error", err.Error()))
	} else {
		s.PlacedAt = placed
	}
	if err := f.st.IndexOrder(ctx, s); err != nil {
		f.log.ErrorContext(ctx, "checkout: order index write failed",
			slog.String("session", s.ID),
			slog.String("order", order.UUID),
			slog.String("error", err.Error()))
	}

	if !order.IsOnlinePayment {
		s.Status = StatusCompleted
		return OrderMayExist, nil
	}
	if !repeated {
		return OrderMayExist, f.startPayment(ctx, t, s)
	}
	return OrderMayExist, f.follow(ctx, t, s, order)
}

// follow takes an online order Django answered to a repeated send as it
// stands: paid, the checkout completes; canceled, it is canceled; still
// awaiting its payment, it gets a payment link only where the checkout
// would offer one (payFor).
func (f *Flow) follow(
	ctx context.Context, t *tenant.Tenant, s *Session, o *django.Order,
) error {
	s.Status = StatusRequiresEscalation
	s.FollowOrder(o.Status, o.PaymentStatus)
	if s.Status != StatusRequiresEscalation {
		return nil
	}
	return f.payFor(ctx, t, s, o)
}

// createOrder sends the session's order, and sends it again while
// Django leaves the outcome unknown, as f.retry allows. Every attempt
// shares one upstream timeout, the order's step of the completion's
// budget, so a repeat follows only a quick failure (a refused or reset
// connection, a 5xx): one that timed out has used the step. What is
// still unknown then is resumed by the next complete.
//
// An outcome left unknown is ErrOrderOutcomeUnknown. A refusal of a
// first send is Django's answer (refused). A repeated send (any after
// the first, and every send of a resumed checkout: repeated) is
// answered by the order the checkout placed, if any, but only once it
// reaches the view: a throttle or a permission refuses it before the
// view looks (beforeTheView), so such a refusal of a repeat says
// nothing about the order. It is unknown, and not sent again here: a
// throttle's budget does not come back within the step.
func (f *Flow) createOrder(
	ctx context.Context, t *tenant.Tenant, s *Session,
	body django.OrderCreate, resumed bool,
) (order *django.Order, repeated bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, f.dj.Timeout())
	defer cancel()
	for attempt := 1; ; attempt++ {
		repeated = resumed || attempt > 1
		order, err = f.dj.CreateOrder(ctx, t.Domain, t.DefaultLocale,
			s.CartID, body)
		switch {
		case err == nil:
			return order, repeated, nil
		case repeated && beforeTheView(err):
			return nil, repeated, fmt.Errorf("%w: %w",
				ErrOrderOutcomeUnknown, err)
		case refused(err):
			return nil, repeated, err
		case attempt >= f.retry.Attempts:
			return nil, repeated, fmt.Errorf("%w: %w",
				ErrOrderOutcomeUnknown, err)
		}
		f.log.WarnContext(ctx, "checkout: order unanswered, sending it again",
			slog.String("session", s.ID),
			slog.Int("attempt", attempt),
			slog.String("error", err.Error()))
		select {
		case <-ctx.Done():
			return nil, repeated, fmt.Errorf("%w: %w",
				ErrOrderOutcomeUnknown, err)
		case <-time.After(f.retry.Delay):
		}
	}
}

// ResumePayment mints the hosted payment link of an escalated order
// whose first link could not be created, while the order can still be
// paid: its payment hold (paymentHoldExpiresAt) is ahead, and the
// checkout does not tell the buyer to wait for the store
// (ErrPaymentAwaitsStore: a payment being processed, a charge awaiting
// review, or no hold at all). Once the hold has passed the checkout already says the store cancels the
// order and the shopper owes nothing, so no link is minted then, though Django
// would still mint one until its longest hold: ErrPaymentWindowClosed. A
// session that already has a link, or no order, is left as is.
func (f *Flow) ResumePayment(
	ctx context.Context, t *tenant.Tenant, s *Session,
) error {
	if s.Status != StatusRequiresEscalation || s.OrderID == 0 ||
		s.PaymentURL != "" {
		return nil
	}
	o, err := f.dj.PlacedOrder(ctx, t.Domain, t.DefaultLocale, s.OrderUUID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPaymentSessionFailed, err)
	}
	return f.payFor(ctx, t, s, o)
}

// payFor mints the payment link of an order awaiting its online payment,
// as Django last told it (o), where the checkout offers one: see
// ResumePayment.
func (f *Flow) payFor(
	ctx context.Context, t *tenant.Tenant, s *Session, o *django.Order,
) error {
	// The rule the checkout is rendered by (ucp.awaitingPayment): no
	// link where it tells the buyer not to pay.
	if o.PaymentStatus == django.PaymentStatusProcessing ||
		o.PaymentAwaitsReview {
		return fmt.Errorf("%w: payment %s, awaits review %t",
			ErrPaymentAwaitsStore, o.PaymentStatus, o.PaymentAwaitsReview)
	}
	hold, err := o.HoldExpiresAt()
	if err != nil {
		return fmt.Errorf("%w: order %s hold expires at %q: %w",
			ErrPaymentSessionFailed, o.UUID, o.PaymentHoldExpiresAt, err)
	}
	// No hold: Django holds no payment open for the order, and the
	// checkout says the store handles it.
	if hold.IsZero() {
		return fmt.Errorf("%w: no payment hold", ErrPaymentAwaitsStore)
	}
	if !hold.After(time.Now()) {
		return fmt.Errorf("%w: its payment hold ended at %q",
			ErrPaymentWindowClosed, o.PaymentHoldExpiresAt)
	}
	return f.startPayment(ctx, t, s)
}

// startPayment creates the Viva hosted checkout for a placed order: the
// buyer authorizes there and portal-configured success URLs land back on
// the storefront. The API requires the URL fields even though Viva's are
// static in the merchant portal. The session stays escalated until the
// payment event lands.
func (f *Flow) startPayment(
	ctx context.Context, t *tenant.Tenant, s *Session,
) error {
	s.Status = StatusRequiresEscalation
	lang := django.Language(ctx, t.TenantConfig)
	links := f.site.Links(t.Domain, lang)
	cs, err := f.dj.CreateOrderCheckoutSession(ctx, t.Domain, lang,
		s.OrderID, s.OrderUUID,
		links.OrderSuccess(s.OrderUUID), links.Cart())
	if apiErr, ok := errors.AsType[*django.APIError](err); ok &&
		(apiErr.Code == django.CodePaymentWindowExpired ||
			apiErr.Code == django.CodeOrderNotPayable) {
		s.PaymentURL = ""
		return fmt.Errorf("%w: %w", ErrPaymentWindowClosed, err)
	}
	if err != nil {
		s.PaymentURL = ""
		return fmt.Errorf("%w: %w", ErrPaymentSessionFailed, err)
	}
	s.PaymentURL = cs.CheckoutURL
	return nil
}

// PlacedOrder names the order a completion of s placed, or "" when none
// is known. A session in progress may not name its order though it was
// placed: the completion's last save failed. The order was recorded
// beside the session as soon as Django returned it (Store.IndexOrder),
// and that record is read here.
func (f *Flow) PlacedOrder(ctx context.Context, s *Session) string {
	if s.OrderUUID != "" {
		return s.OrderUUID
	}
	placed, err := f.st.placedOrder(ctx, s.Schema, s.ID)
	if err != nil {
		f.log.ErrorContext(ctx, "checkout: placed order read failed",
			slog.String("session", s.ID),
			slog.String("error", err.Error()))
		return ""
	}
	return placed
}

// ApplyOrderEvent folds a Django order/payment event into the checkout
// that placed the order, if that session still exists: sessions expire
// long before orders stop changing, and a missing one is not an error.
//
// It takes no lock: its save is a compare-and-set like every other, and
// a save that lost returns ErrSessionChanged for the caller to retry. A
// lock would answer "busy" to every event while a crashed pod's lock
// lasted, for nothing the compare-and-set does not already give. An event
// that lands while the completion that placed its order is still
// finishing is stored too: that completion stores its own outcome over
// it (saveOverEvent).
//
// What an event does, by the session it finds:
//   - a terminal session: nothing;
//   - a session that names no order: any event records its order, read
//     from Django by the event's uuid (orderUUID, which the order's link
//     to this checkout vouches for): its id, and its creation time, from
//     which the session's day counts. An order Django no longer has is
//     left unrecorded and not asked for again;
//   - the event's order status and payment status set the session's
//     (Session.FollowOrder): a checkout awaiting its payment is canceled
//     with its order, or completed with its payment. An event is a
//     snapshot Django took, and the states it can move a session to are
//     final, so one delivered late or out of order moves it to a state
//     the order did reach.
func (f *Flow) ApplyOrderEvent(
	ctx context.Context,
	schema, checkoutID, orderUUID, orderStatus, paymentStatus string,
) error {
	// The order's link names the checkout, of either protocol.
	s, err := f.st.LoadAnyProtocol(ctx, schema, checkoutID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.Terminal() {
		return nil
	}
	changed := false
	if s.OrderUUID == "" {
		// An event names no language, and only the order's id and
		// creation time are read.
		order, err := f.dj.PlacedOrder(ctx, s.Domain, "", orderUUID)
		if errors.Is(err, django.ErrNotFound) {
			// Django no longer has the order the event was for: no
			// retry brings it back, and the session is left as it is.
			f.log.WarnContext(ctx,
				"checkout: order event for an order that is gone",
				slog.String("session", s.ID),
				slog.String("order", orderUUID))
			return nil
		}
		if err != nil {
			return err
		}
		placed, err := order.Created()
		if err != nil {
			return fmt.Errorf("checkout: %w", err)
		}
		s.OrderID, s.OrderUUID = order.ID, order.UUID
		s.PlacedAt = placed
		changed = true
	}
	if s.FollowOrder(orderStatus, paymentStatus) {
		changed = true
	}
	if !changed {
		return nil
	}
	return f.st.Save(ctx, s)
}
