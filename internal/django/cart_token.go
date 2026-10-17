package django

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
)

// maxCartTokenLen bounds a forwarded cart token. Django mints
// secrets.token_urlsafe(32), 43 characters; anything far longer is not
// one of its tokens.
const maxCartTokenLen = 128

// ErrInvalidCartToken marks a cart token that is not shaped like one
// Django mints: client input, refused before it is sent anywhere.
var ErrInvalidCartToken = errors.New("django: malformed cart token")

// CartToken is a signed-in shopper's cart pass: Django mints it for the
// storefront (POST /cart/agent-token), the chat widget sends it with a
// turn, and the gateway only forwards it, as X-Cart-Token on the cart
// routes Django accepts it on. It is a credential for that cart, so it
// rides the request context and nothing else: never the stored
// conversation, the model's prompt or tool arguments, or a log line
// (String, GoString and LogValue redact it).
type CartToken struct {
	value   string
	refused atomic.Bool
}

// ParseCartToken accepts a token shaped like Django's: URL-safe base64,
// bounded length. Django is the one that decides whether it is valid.
func ParseCartToken(raw string) (*CartToken, error) {
	if raw == "" || len(raw) > maxCartTokenLen {
		return nil, ErrInvalidCartToken
	}
	for i := range len(raw) {
		switch c := raw[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z',
			c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return nil, ErrInvalidCartToken
		}
	}
	return &CartToken{value: raw}, nil
}

// Refused reports whether Django refused the token during this turn
// (unknown, expired or revoked), so the storefront mints a new one.
func (t *CartToken) Refused() bool { return t.refused.Load() }

func (t *CartToken) String() string       { return "[redacted]" }
func (t *CartToken) GoString() string     { return "[redacted]" }
func (t *CartToken) LogValue() slog.Value { return slog.StringValue("[redacted]") }

type cartTokenKey struct{}

// WithCartToken scopes every cart call made with ctx to the token's cart.
func WithCartToken(ctx context.Context, t *CartToken) context.Context {
	return context.WithValue(ctx, cartTokenKey{}, t)
}

// HasCartToken reports whether cart calls made with ctx act on a
// signed-in shopper's cart, whatever cart id they name.
func HasCartToken(ctx context.Context) bool {
	_, ok := cartTokenFrom(ctx)
	return ok
}

func cartTokenFrom(ctx context.Context) (*CartToken, bool) {
	t, ok := ctx.Value(cartTokenKey{}).(*CartToken)
	return t, ok && t != nil
}
