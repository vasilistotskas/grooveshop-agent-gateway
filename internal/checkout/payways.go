package checkout

import (
	"context"
	"errors"
	"fmt"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// ErrPayWayUnavailable marks a selected payment method the store does not
// offer for the session's delivery (carrier and kind).
var ErrPayWayUnavailable = errors.New(
	"checkout: the selected payment method is not available for this " +
		"delivery")

// PayWaysFor lists the active payment methods Django offers for a
// delivery. Scoping by carrier and kind applies Django's own two-layer
// filter: BOX NOW PAY ON THE GO is card-at-the-locker and belongs to no
// courier round, while courier cash belongs to no locker. The rules live
// upstream, per carrier; duplicating them here would be a second place to
// get them wrong. Until the fulfillment is complete the list is
// unfiltered, and completion re-checks against the final delivery.
func PayWaysFor(
	ctx context.Context, dj *django.Client, t *tenant.Tenant, f Fulfillment,
) ([]django.PayWay, error) {
	provider, kind := "", ""
	if f.Complete() {
		provider, kind = f.ProviderCode, f.Kind
	}
	page, err := dj.PayWays(ctx, t.Domain, t.DefaultLocale, provider, kind)
	if err != nil {
		return nil, fmt.Errorf("checkout: pay ways: %w", err)
	}
	return page.Results, nil
}

// OfflinePayWay returns the first active collect-later method for the
// delivery — the one an agent settles without a PSP — or nil when the
// store offers none. IsCollectedLater rather than "not online": an absent
// or unrecognised settlement must exclude a pay way, not select it.
func OfflinePayWay(
	ctx context.Context, dj *django.Client, t *tenant.Tenant, f Fulfillment,
) (*django.PayWay, error) {
	pws, err := PayWaysFor(ctx, dj, t, f)
	if err != nil {
		return nil, err
	}
	for i := range pws {
		if pws[i].Active && pws[i].IsCollectedLater() {
			return &pws[i], nil
		}
	}
	return nil, nil
}

// availablePayWay returns the session's selected pay way if the store
// offers it for the session's delivery.
func availablePayWay(
	ctx context.Context, dj *django.Client, t *tenant.Tenant, s *Session,
) (*django.PayWay, error) {
	pws, err := PayWaysFor(ctx, dj, t, s.Fulfillment)
	if err != nil {
		return nil, err
	}
	for i := range pws {
		if pws[i].ID == s.PayWayID && pws[i].Active {
			return &pws[i], nil
		}
	}
	return nil, ErrPayWayUnavailable
}
