package checkout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// ErrNotReady marks a complete call on a session that still misses data.
var ErrNotReady = errors.New("checkout: session is not ready to complete")

// ErrPaymentMethodUnsupported marks a pay way the agentic surface cannot
// finish (Stripe tokenized completion ships behind the per-tenant flag).
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

// ResumePayment mints a fresh link; callers must never report it as a
// failed order — the buyer would check out again and order twice.
var ErrPaymentSessionFailed = errors.New(
	"checkout: the order is placed but its payment link could not be " +
		"created")

// Outcome describes what completing a session produced.
type Outcome struct {
	// Escalated: the buyer must authorize payment at PaymentURL (Viva
	// hosted checkout). The session stays open until the webhook lands.
	Escalated  bool
	PaymentURL string

	// Completed: the order is placed (offline pay ways — e.g. cash on
	// delivery — need no payment authorization).
	Completed bool
}

// Flow orchestrates order creation over the existing Django endpoints.
type Flow struct {
	dj  *django.Client
	st  *Store
	log *slog.Logger
}

func NewFlow(dj *django.Client, st *Store, log *slog.Logger) *Flow {
	return &Flow{dj: dj, st: st, log: log}
}

// Complete places the order for a ready session. The caller holds the
// session lock and the completion claim; Complete mutates the session and
// the caller persists it.
func (f *Flow) Complete(
	ctx context.Context, t *tenant.Tenant, s *Session,
) (*Outcome, error) {
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
	// supported; tokenized card completion (Stripe) arrives with the ACP
	// delegated payment flag.
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

	// Persisted BEFORE the order exists. The session lock can lapse
	// during the upstream calls below, and a second complete that loads
	// the session then must see a completion in flight — not a ready
	// session it would place a second time.
	s.Status = StatusCompleteInProgress
	if err := f.st.Save(ctx, s); err != nil {
		s.Status = StatusReadyForComplete
		return nil, err
	}
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
			ShippingProviderCode: s.Fulfillment.ProviderCode,
			ShippingKind:         s.Fulfillment.Kind,
			AcsStationExternalID: s.Fulfillment.AcsStationExternalID,
			AcsStationBranch:     s.Fulfillment.AcsStationBranch,
			BoxnowLockerID:       s.Fulfillment.BoxnowLockerID,
			BoxnowCompartmentSz:  s.Fulfillment.BoxnowCompartmentSize,
		})
	if err != nil {
		if !refused(err) {
			return nil, fmt.Errorf("%w: %w", ErrOrderOutcomeUnknown, err)
		}
		s.Status = StatusReadyForComplete
		return nil, err
	}
	s.OrderID = order.ID
	s.OrderUUID = order.UUID
	if err := f.st.IndexOrder(ctx, s); err != nil {
		f.log.ErrorContext(ctx, "checkout: order index write failed",
			slog.String("order", order.UUID),
			slog.String("error", err.Error()))
	}

	if !payWay.IsOnlineSettlement() {
		s.Status = StatusCompleted
		return &Outcome{Completed: true}, nil
	}
	return f.startPayment(ctx, t, s)
}

// ResumePayment mints a fresh hosted payment link for an escalated order
// whose first link could not be created. A session that already has a
// link, or no order, is returned as is.
func (f *Flow) ResumePayment(
	ctx context.Context, t *tenant.Tenant, s *Session,
) (*Outcome, error) {
	if s.Status != StatusRequiresEscalation || s.OrderID == 0 ||
		s.PaymentURL != "" {
		return &Outcome{Escalated: true, PaymentURL: s.PaymentURL}, nil
	}
	return f.startPayment(ctx, t, s)
}

// startPayment creates the Viva hosted checkout for a placed order: the
// buyer authorizes there and portal-configured success URLs land back on
// the storefront. The API requires the URL fields even though Viva's are
// static in the merchant portal.
func (f *Flow) startPayment(
	ctx context.Context, t *tenant.Tenant, s *Session,
) (*Outcome, error) {
	s.Status = StatusRequiresEscalation
	cs, err := f.dj.CreateOrderCheckoutSession(ctx, t.Domain, t.DefaultLocale,
		s.OrderID, s.OrderUUID,
		storefront.OrderSuccess(t.Domain, s.OrderUUID),
		storefront.Cart(t.Domain))
	if err != nil {
		s.PaymentURL = ""
		return nil, fmt.Errorf("%w: %w", ErrPaymentSessionFailed, err)
	}
	s.PaymentURL = cs.CheckoutURL
	return &Outcome{Escalated: true, PaymentURL: cs.CheckoutURL}, nil
}

// ApplyOrderEvent folds a Django order/payment event into the checkout
// that placed the order, if that session still exists — sessions expire
// long before orders stop changing, and a missing one is not an error.
// It takes the session lock like every other mutation: an unlocked write
// raced a concurrent cancel or complete, and the last writer won. A busy
// session returns ErrLocked for the caller to retry.
func (f *Flow) ApplyOrderEvent(
	ctx context.Context, schema, checkoutID, paymentStatus string,
) error {
	if paymentStatus != django.PaymentStatusCompleted {
		return nil
	}
	release, err := f.st.Lock(ctx, schema, checkoutID)
	if err != nil {
		return err
	}
	defer release()
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
	s.Status = StatusCompleted
	return f.st.Save(ctx, s)
}
