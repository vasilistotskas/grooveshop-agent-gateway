package django

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/text"
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
	// ErrCartUnreachable: Django served another cart than the one named.
	// It never refuses a cart an anonymous caller cannot reach (a
	// signed-in shopper's, or a guest cart since deleted): it serves a
	// new guest cart in its place, and acts on that one.
	ErrCartUnreachable = errors.New("django: cart unreachable")
	// ErrCartTokenRefused: Django refused the signed-in shopper's cart
	// token (unknown, expired or revoked). The cart was not reached, so
	// it is ErrCartUnreachable too.
	ErrCartTokenRefused = fmt.Errorf(
		"django: cart token refused: %w", ErrCartUnreachable)
)

// APIError is a Django error response (docs/error-contract.md in the
// Django repository). It unwraps to the matching sentinel so callers
// branch with errors.Is.
//
//   - Code: the stable snake_case error code, empty when the body has
//     none (a 5xx from a proxy, a bare list body).
//   - Detail: the store's own explanation, in the request's language: its
//     `detail`, or each field's messages for a validation error. Empty when
//     the body gave neither, so it is always safe to show.
//   - Reason: the machine-readable code some 4xx bodies carry next to
//     detail (coupon, loyalty and gift-card rejections).
//   - RetryAfter: the throttle's Retry-After, zero when absent.
//   - Body: the raw (bounded) response, for the few callers that decode a
//     structured failure, such as the stock-shortfall 409.
type APIError struct {
	Status     int
	Code       string
	Detail     string
	Reason     string
	RetryAfter time.Duration
	Body       []byte
}

func (e *APIError) Error() string {
	// A body without detail or fields (a bare list, a proxy page) is
	// still quoted, bounded: an error log needs something to go on.
	detail := e.Detail
	if detail == "" {
		detail = text.Ellipsize(string(e.Body), 512)
	}
	if detail == "" {
		detail = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return fmt.Sprintf("django: status %d (%s): %s",
			e.Status, e.Code, detail)
	}
	return fmt.Sprintf("django: status %d: %s", e.Status, detail)
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
