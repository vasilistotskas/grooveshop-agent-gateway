package checkout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// ErrNotReady marks a complete call on a session that still misses data.
var ErrNotReady = errors.New("checkout: session is not ready to complete")

// ErrPaymentMethodUnsupported marks a pay way the agentic surface cannot
// finish (Stripe tokenized completion ships behind the per-tenant flag).
var ErrPaymentMethodUnsupported = errors.New(
	"checkout: this payment method cannot be completed by an agent yet")

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
	if s.Status != StatusReadyForComplete {
		return nil, fmt.Errorf("%w: missing %v", ErrNotReady, s.Missing())
	}

	payWay, err := f.dj.PayWayByID(ctx, t.Domain, t.DefaultLocale, s.PayWayID)
	if err != nil {
		return nil, fmt.Errorf("checkout: pay way lookup: %w", err)
	}
	// Viva's hosted authorization and offline pay ways are supported;
	// tokenized card completion (Stripe) arrives with the ACP delegated
	// payment flag.
	online := payWay.IsOnlinePayment
	if online && payWay.ProviderCode != "viva_wallet" {
		return nil, ErrPaymentMethodUnsupported
	}

	if _, err := f.dj.ReserveStock(
		ctx, t.Domain, t.DefaultLocale, s.CartID,
	); err != nil {
		return nil, err
	}

	s.Status = StatusCompleteInProgress
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
		s.Status = StatusReadyForComplete
		return nil, err
	}
	s.OrderID = order.ID
	s.OrderUUID = order.UUID
	if err := f.st.IndexOrder(ctx, s.Schema, order.UUID, s.ID); err != nil {
		f.log.ErrorContext(ctx, "checkout: order index write failed",
			slog.String("order", order.UUID),
			slog.String("error", err.Error()))
	}

	if !online {
		s.Status = StatusCompleted
		return &Outcome{Completed: true}, nil
	}

	// Viva: the buyer authorizes on the hosted page; portal-configured
	// success URLs land back on the storefront. The API requires the URL
	// fields even though Viva's are static in the merchant portal.
	successURL := fmt.Sprintf("https://%s/checkout/success/%s",
		t.Domain, order.UUID)
	cancelURL := fmt.Sprintf("https://%s/cart", t.Domain)
	cs, err := f.dj.CreateOrderCheckoutSession(ctx, t.Domain, t.DefaultLocale,
		order.ID, order.UUID, successURL, cancelURL)
	if err != nil {
		// The order exists but the payment session failed: keep the
		// escalation pending so a retry can mint a fresh Viva code.
		s.Status = StatusRequiresEscalation
		s.PaymentURL = ""
		return nil, fmt.Errorf("checkout: payment session: %w", err)
	}
	s.Status = StatusRequiresEscalation
	s.PaymentURL = cs.CheckoutURL
	return &Outcome{Escalated: true, PaymentURL: cs.CheckoutURL}, nil
}

// ApplyOrderEvent folds a Django order/payment event into the session it
// belongs to. Returns the updated session (persisted) or nil when no
// session tracks that order.
func (f *Flow) ApplyOrderEvent(
	ctx context.Context, schema, orderUUID, paymentStatus string,
) (*Session, error) {
	s, err := f.st.SessionForOrder(ctx, schema, orderUUID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if s.Terminal() {
		return s, nil
	}
	if paymentStatus == "COMPLETED" {
		s.Status = StatusCompleted
		if err := f.st.Save(ctx, s); err != nil {
			return nil, err
		}
	}
	return s, nil
}
