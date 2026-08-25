package checkout

import (
	"context"
	"errors"
	"strings"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// DiscountRejection is one discount code the store refused, in the ACP
// discount-extension vocabulary (Reason is always a spec enum value).
type DiscountRejection struct {
	Code    string `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// Spec vocabulary for rejected discount codes (ACP discount extension,
// schema.discount.json 2026-01-27).
const (
	DiscountCodeInvalid               = "discount_code_invalid"
	DiscountCodeCombinationDisallowed = "discount_code_combination_disallowed"
)

// specDiscountReasons is the discount_error_codes enum. Reasons outside it
// (Django also emits discount_code_not_started) must be mapped before they
// reach a response, or schema validation on the platform side breaks.
var specDiscountReasons = map[string]bool{
	"discount_code_expired":                true,
	"discount_code_invalid":                true,
	"discount_code_already_applied":        true,
	"discount_code_combination_disallowed": true,
	"discount_code_minimum_not_met":        true,
	"discount_code_user_not_logged_in":     true,
	"discount_code_user_ineligible":        true,
	"discount_code_usage_limit_reached":    true,
}

// mapDiscountReason folds Django rejection reasons onto the spec enum.
// discount_code_not_started has no spec equivalent — a not-yet-active code
// is unusable today, so it reports as invalid while the human-readable
// message keeps the precise explanation.
func mapDiscountReason(reason string) string {
	if specDiscountReasons[reason] {
		return reason
	}
	return DiscountCodeInvalid
}

// ApplyDiscountCodes reconciles the cart's coupon with a submitted code
// list (replace semantics: the submission supersedes whatever was applied
// before; an empty list clears). The store applies ONE coupon per cart, so
// the first code is applied and every extra code is rejected as
// discount_code_combination_disallowed without touching the cart.
//
// A refused code is recorded on the session — it is not a request failure;
// the renderers surface it in discounts.rejected. Only upstream/transport
// failures return an error.
//
// Gift cards are deliberately out of scope here: they are a payment
// instrument, not a discount (the ACP discount extension does not model
// them), and stay off the gateway's agentic surfaces.
func ApplyDiscountCodes(
	ctx context.Context, dj *django.Client, t *tenant.Tenant,
	s *Session, codes []string,
) error {
	s.DiscountCodes = codes
	s.RejectedDiscounts = nil

	if len(codes) == 0 {
		_, err := dj.RemoveCoupon(ctx, t.Domain, t.DefaultLocale, s.CartID)
		return err
	}

	first := strings.TrimSpace(codes[0])
	for _, extra := range codes[1:] {
		s.RejectedDiscounts = append(s.RejectedDiscounts, DiscountRejection{
			Code:   extra,
			Reason: DiscountCodeCombinationDisallowed,
			Message: "Only one discount code can be applied per order; " +
				"the first code was used.",
		})
	}

	_, err := dj.ApplyCoupon(ctx, t.Domain, t.DefaultLocale, s.CartID, first)
	var rejected *django.CouponRejectedError
	if errors.As(err, &rejected) {
		s.RejectedDiscounts = append([]DiscountRejection{{
			Code:    first,
			Reason:  mapDiscountReason(rejected.Reason),
			Message: rejected.Detail,
		}}, s.RejectedDiscounts...)
		// Enforce replace semantics even on refusal: an unknown code is
		// rejected before Django detaches the previous coupon, so clear
		// it explicitly (best effort — the next render reflects reality).
		_, _ = dj.RemoveCoupon(ctx, t.Domain, t.DefaultLocale, s.CartID)
		return nil
	}
	return err
}
