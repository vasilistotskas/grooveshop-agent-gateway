package django

import (
	"errors"
	"fmt"
)

// Sentinel errors shared by every protocol surface. Handlers map these to
// MCP tool errors, UCP/ACP error payloads, or HTTP statuses — never raw
// upstream bodies.
var (
	ErrNotFound     = errors.New("django: not found")
	ErrConflict     = errors.New("django: conflict")
	ErrValidation   = errors.New("django: invalid request")
	ErrThrottled    = errors.New("django: throttled")
	ErrUpstreamDown = errors.New("django: upstream unavailable")
	// ErrUnauthorized: the bearer token is missing, invalid, or expired.
	ErrUnauthorized = errors.New("django: unauthorized")
	// ErrForbidden: the credential is valid but lacks the required scope
	// or permission.
	ErrForbidden = errors.New("django: forbidden")
)

// APIError carries the upstream status and DRF "detail" message. It unwraps
// to the matching sentinel so callers branch with errors.Is. Reason is the
// optional machine-readable code some 4xx bodies carry next to detail
// (coupon, loyalty and gift-card rejections). Body is the raw (bounded)
// response for the few callers that decode a structured failure, such as
// the stock-shortfall 409.
type APIError struct {
	Status int
	Detail string
	Reason string
	Body   []byte
}

func (e *APIError) Error() string {
	return fmt.Sprintf("django: status %d: %s", e.Status, e.Detail)
}

// CouponRejectedError is a coupon apply refused by the store's promotion
// engine: Reason is Django's machine-readable rejection code (the ACP
// discount-extension vocabulary, plus discount_code_not_started), Detail
// the human-readable explanation. It unwraps to ErrValidation so generic
// handlers keep treating it as a 400-class failure.
type CouponRejectedError struct {
	Reason string
	Detail string
}

func (e *CouponRejectedError) Error() string {
	return fmt.Sprintf("django: coupon rejected (%s): %s", e.Reason, e.Detail)
}

func (e *CouponRejectedError) Unwrap() error { return ErrValidation }

func (e *APIError) Unwrap() error {
	switch {
	case e.Status == 401:
		return ErrUnauthorized
	case e.Status == 403:
		return ErrForbidden
	case e.Status == 404:
		return ErrNotFound
	case e.Status == 409:
		return ErrConflict
	case e.Status == 429:
		return ErrThrottled
	case e.Status >= 500:
		return ErrUpstreamDown
	case e.Status >= 400:
		return ErrValidation
	}
	return nil
}
