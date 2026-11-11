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

// ErrPaymentMethodUnsupported marks a pay way the agentic surface cannot
// finish: any online pay way other than Viva hosted authorization (Stripe
// included), or one whose settlement is unknown.
var ErrPaymentMethodUnsupported = errors.New(
	"checkout: this payment method cannot be completed by an agent yet")

// ErrOrderOutcomeUnknown marks an order creation whose result never
// arrived — a timeout, a dropped connection, a 5xx after the commit, an
// unreadable success. The order may exist, and Django keeps the cart of
// an order awaiting online payment, so a retry could place it twice: the
// session stays complete_in_progress and callers must not free the
// completion for another attempt.
var ErrOrderOutcomeUnknown = errors.New(
	"checkout: the store did not confirm whether the order was placed")

// ErrPaymentSessionFailed marks an order that exists but whose hosted
// payment link could not be created. The session stays escalated and
// ResumePayment mints a fresh link; callers must never report it as a
// failed order — the buyer would check out again and order twice.
var ErrPaymentSessionFailed = errors.New(
	"checkout: the order is placed but its payment link could not be " +
		"created")

// ErrSessionNotSaved marks an order that is placed while its session
// could not be stored: the stored session still reads
// complete_in_progress.
var ErrSessionNotSaved = errors.New(
	"checkout: the order is placed but its session could not be saved")

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

// Placement is what a completion did about the order. It decides the
// caller's idempotency key, whatever error came with it.
type Placement int

const (
	// OrderMayExist: an order exists, or may. The caller spends its key,
	// so a retry re-renders the session and never places a second order.
	// It is the zero value: a Placement nobody set must not release a
	// key.
	OrderMayExist Placement = iota
	// NothingPlaced: refused, and nothing was created. The caller
	// releases its key, so the same key can try again.
	NothingPlaced
)

// completionSteps sizes the part of Complete that runs without its
// caller, in upstream timeouts: one for the order, one for its payment
// session, one for the Redis writes around them. The Django client cuts
// each of its calls at one upstream timeout by itself, so however slow
// Django answers, the writes keep their share.
const completionSteps = 3

// Flow orchestrates order creation over the existing Django endpoints.
type Flow struct {
	dj     *django.Client
	st     *Store
	site   storefront.Site
	budget time.Duration
	log    *slog.Logger
}

func NewFlow(
	dj *django.Client, st *Store, site storefront.Site, log *slog.Logger,
) *Flow {
	return &Flow{
		dj: dj, st: st, site: site,
		budget: completionSteps * dj.Timeout(), log: log,
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
// for get_order or the order webhooks to find it by.
func (f *Flow) Complete(
	ctx context.Context, t *tenant.Tenant, s *Session,
) (Placement, error) {
	payWay, err := f.admit(ctx, t, s)
	if err != nil {
		// Nothing is stored for a session that was not admitted. A
		// snapshot that reads in progress is another attempt's, and
		// saving it would overwrite that attempt's session.
		return NothingPlaced, err
	}
	// No order exists yet, so a call whose context has already ended
	// (a caller that left, a server shutting down) gets none.
	if err := ctx.Err(); err != nil {
		return NothingPlaced, err
	}

	// From here on the caller's cancellation no longer applies.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), f.budget)
	defer cancel()

	// Persisted BEFORE the order exists, and only over the session this
	// call loaded: of the completions that read one ready session, Redis
	// lets a single one through here. A later complete then loads a
	// completion in flight, not a ready session to place a second time.
	s.Status = StatusCompleteInProgress
	if err := f.st.Save(ctx, s); err != nil {
		// The write that guards the order was refused, by Redis failing
		// or by a session that has moved on (ErrSessionChanged): no
		// order is sent, and the session is not written again.
		s.Status = StatusReadyForComplete
		return NothingPlaced, err
	}
	placement, err := f.place(ctx, t, s, payWay)
	// Stored as the order call left it: unconfirmed (still in progress,
	// and kept as long as a placed one, since an order may exist),
	// placed, or ready again after Django refused the order.
	saveErr := f.st.Save(ctx, s)
	switch {
	case saveErr == nil:
	case placement == NothingPlaced:
		// The refusal is definite, and a session left in progress would
		// claim an order that cannot exist: one more attempt. If it
		// fails too, the in-progress session ends with the short
		// lifetime of an open one (Session.ttl), not a placed one's.
		saveErr = f.st.Save(ctx, s)
	case errors.Is(saveErr, ErrSessionChanged):
		saveErr = f.saveOverEvent(ctx, s)
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
// refuses a session in progress, so that other writer is an order event
// (ApplyOrderEvent), which records the order this completion placed and
// may complete the session when the payment has landed. The completion
// knows more (the payment page it opened), so its outcome is stored over
// the event's, keeping "completed" if the event said the order is paid.
func (f *Flow) saveOverEvent(ctx context.Context, s *Session) error {
	stored, err := f.st.Load(ctx, s.Schema, s.ID)
	if err != nil {
		return err
	}
	switch stored.Status {
	case StatusCompleted:
		s.Status = StatusCompleted
	case StatusCompleteInProgress:
	default:
		// Not what an event leaves: nothing is written over it.
		return ErrSessionChanged
	}
	s.Revision = stored.Revision
	return f.st.Save(ctx, s)
}

// CompletionDeadline is the latest a completion of s can still run, for a
// session stored in progress: its budget starts just before that save,
// which set s.UpdatedAt. A session still in progress after it stays so
// until it expires, because its outcome was never recorded. The writer's
// clock sets it and the reader's judges it: a reader ahead says "outcome
// unknown" early, which is the safe wrong answer. A writer ahead stamps a
// save in the reader's future; no completion runs longer than its budget
// from now, so the deadline is never further away than that.
func (f *Flow) CompletionDeadline(s *Session) time.Time {
	saved := s.UpdatedAt
	if now := time.Now(); saved.After(now) {
		saved = now
	}
	return saved.Add(f.budget)
}

// admit checks that the session can be placed and reserves its stock.
// No order exists yet, so it runs on the caller's context.
func (f *Flow) admit(
	ctx context.Context, t *tenant.Tenant, s *Session,
) (*django.PayWay, error) {
	switch s.Status {
	case StatusReadyForComplete:
	case StatusCompleteInProgress:
		return nil, ErrCompletionInProgress
	default:
		return nil, fmt.Errorf("%w: missing %v", ErrNotReady, s.Missing())
	}

	// Checked against the FINAL delivery: a method selected before the
	// carrier was chosen may be one that carrier cannot settle.
	payWay, err := availablePayWay(ctx, f.dj, t, s)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf(
			"%w: pay way %d reports settlement %q",
			ErrPaymentMethodUnsupported, payWay.ID, payWay.Settlement,
		)
	}
	if payWay.IsOnlineSettlement() {
		if payWay.ProviderCode != django.ProviderVivaWallet {
			return nil, ErrPaymentMethodUnsupported
		}
		// The hosted-payment gate is read at selection AND here: it can
		// be switched off in between, and a gated method is refused,
		// never honoured from a stale selection.
		if !t.HostedPaymentOn() {
			return nil, fmt.Errorf(
				"%w: hosted payment is disabled for this store",
				ErrPaymentMethodUnsupported)
		}
	}

	if _, err := f.dj.ReserveStock(
		ctx, t.Domain, t.DefaultLocale, s.CartID,
	); err != nil {
		return nil, err
	}
	return payWay, nil
}

// place creates the order for a session stored in progress, links it,
// and starts its payment when the pay way is settled online. It leaves
// the session as the result made it; Complete stores it.
func (f *Flow) place(
	ctx context.Context, t *tenant.Tenant, s *Session, payWay *django.PayWay,
) (Placement, error) {
	order, err := f.dj.CreateOrder(ctx, t.Domain, t.DefaultLocale, s.CartID,
		django.OrderCreate{
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
			AcsStationExternalID: s.Fulfillment.AcsStationExternalID,
			AcsStationBranch:     s.Fulfillment.AcsStationBranch,
			BoxnowLockerID:       s.Fulfillment.BoxnowLockerID,
			// Every UCP and ACP checkout response links the store's terms
			// and privacy policy for the platform to present to the buyer
			// (storefront.Terms, storefront.Privacy); completing it is the
			// buyer's acceptance. Django records it as an agent checkout,
			// proven by the internal secret.
			TermsAccepted: true,
			Attribution:   agentAttribution(s.Protocol),
		})
	if err != nil {
		if !refused(err) {
			s.Unconfirmed = true
			return OrderMayExist,
				fmt.Errorf("%w: %w", ErrOrderOutcomeUnknown, err)
		}
		s.Status = StatusReadyForComplete
		return NothingPlaced, err
	}
	s.OrderID = order.ID
	s.OrderUUID = order.UUID
	s.PlacedAt = time.Now().UTC()
	if err := f.st.IndexOrder(ctx, s); err != nil {
		f.log.ErrorContext(ctx, "checkout: order index write failed",
			slog.String("order", order.UUID),
			slog.String("error", err.Error()))
	}

	if !payWay.IsOnlineSettlement() {
		s.Status = StatusCompleted
		return OrderMayExist, nil
	}
	return OrderMayExist, f.startPayment(ctx, t, s)
}

// ResumePayment mints a fresh hosted payment link for an escalated order
// whose first link could not be created. A session that already has a
// link, or no order, is left as is.
func (f *Flow) ResumePayment(
	ctx context.Context, t *tenant.Tenant, s *Session,
) error {
	if s.Status != StatusRequiresEscalation || s.OrderID == 0 ||
		s.PaymentURL != "" {
		return nil
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
	return placed.OrderUUID
}

// ApplyOrderEvent folds a Django order/payment event into the checkout
// that placed the order, if that session still exists: sessions expire
// long before orders stop changing, and a missing one is not an error.
//
// It takes no lock: its save is a compare-and-set like every other, and
// a save that lost returns ErrSessionChanged for the caller to retry. A
// lock would answer "busy" to every push while a crashed pod's lock
// lasted, and Django's few retries can all fall inside that. An event
// that lands while the completion that placed its order is still
// finishing is stored too: that completion stores its own outcome over
// it (saveOverEvent).
//
// What an event does, by the session it finds:
//   - a terminal session: nothing;
//   - a session that names no order: any event records the order the
//     completion placed (its id, uuid and time, from the record beside
//     the session). Without that record the gateway does not know the
//     order's id, and the session is left as it is;
//   - a payment-completed event also completes a session that names its
//     order. No other event changes a status.
func (f *Flow) ApplyOrderEvent(
	ctx context.Context, schema, checkoutID, paymentStatus string,
) error {
	s, err := f.st.Load(ctx, schema, checkoutID)
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
		placed, err := f.st.placedOrder(ctx, schema, checkoutID)
		if err != nil {
			return err
		}
		if placed.OrderUUID == "" {
			f.log.WarnContext(ctx,
				"checkout: order event for a session with no placed order",
				slog.String("session", s.ID))
			return nil
		}
		s.OrderID, s.OrderUUID = placed.OrderID, placed.OrderUUID
		s.PlacedAt = placed.PlacedAt
		changed = true
	}
	if paymentStatus == django.PaymentStatusCompleted {
		s.Status = StatusCompleted
		changed = true
	}
	if !changed {
		return nil
	}
	return f.st.Save(ctx, s)
}
