package django

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCartToken is shaped like secrets.token_urlsafe(32).
const testCartToken = "Zk3n_Q8vX2-rT5yLmW9aB4cD7eF0gH1iJ6kN8pS3uVw"

func tokenContext(t *testing.T) (context.Context, *CartToken) {
	t.Helper()
	tok, err := ParseCartToken(testCartToken)
	require.NoError(t, err)
	return WithCartToken(context.Background(), tok), tok
}

// cartCalls makes every call on the routes Django accepts the token on,
// naming a cart other than the one the fixtures serve.
func cartCalls(ctx context.Context, c *Client) map[string]error {
	const host, named = "shop.example.test",
		"6f1c1f3e-2d6a-4b0f-9a3e-2f6d6c1b9a10"
	errs := map[string]error{}
	_, errs["GetCart"] = c.GetCart(ctx, host, "el", named)
	_, errs["AddCartItem"] = c.AddCartItem(ctx, host, "el", named, 1, 1)
	_, errs["UpdateCartItem"] = c.UpdateCartItem(ctx, host, "el", named, 410, 2)
	errs["RemoveCartItem"] = c.RemoveCartItem(ctx, host, "el", named, 410)
	_, errs["ApplyCoupon"] = c.ApplyCoupon(ctx, host, "el", named, "SAVE10")
	_, errs["RemoveCoupon"] = c.RemoveCoupon(ctx, host, "el", named)
	return errs
}

func cartFixtureServer(
	t *testing.T, seen func(*http.Request),
) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen(r)
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodDelete &&
				strings.HasPrefix(r.URL.Path, "/api/v1/cart/item/"):
				w.WriteHeader(http.StatusNoContent)
			case strings.HasPrefix(r.URL.Path, "/api/v1/cart/item"):
				_, _ = w.Write(fixture(t, "cart_item_created.json"))
			default:
				_, _ = w.Write(fixture(t, "cart_empty.json"))
			}
		}))
	t.Cleanup(srv.Close)
	return srv
}

// With a signed-in shopper's token every cart call sends it instead of
// X-Cart-Id, and the cart Django resolves it to is the answer, whatever
// id the call named: the token never resolves to a substitute.
func TestCartCallsSendTheTokenInsteadOfTheCartID(t *testing.T) {
	var calls atomic.Int32
	srv := cartFixtureServer(t, func(r *http.Request) {
		calls.Add(1)
		assert.Equal(t, testCartToken, r.Header.Get("X-Cart-Token"),
			r.Method+" "+r.URL.Path)
		assert.Empty(t, r.Header.Get("X-Cart-Id"), r.Method+" "+r.URL.Path)
		assert.Equal(t, "test-secret", r.Header.Get("X-Internal-Gateway"))
	})
	ctx, tok := tokenContext(t)

	for name, err := range cartCalls(ctx, newTestClient(t, srv.URL)) {
		assert.NoError(t, err, name)
	}
	assert.Equal(t, int32(6), calls.Load())
	assert.False(t, tok.Refused())
}

// Without a token nothing changes: X-Cart-Id names the cart, no token
// header is sent, and another cart answering is refused.
func TestCartCallsWithoutATokenNameTheCart(t *testing.T) {
	srv := cartFixtureServer(t, func(r *http.Request) {
		_, sent := r.Header["X-Cart-Token"]
		assert.False(t, sent, r.Method+" "+r.URL.Path)
		assert.Equal(t, "6f1c1f3e-2d6a-4b0f-9a3e-2f6d6c1b9a10",
			r.Header.Get("X-Cart-Id"))
	})

	errs := cartCalls(context.Background(), newTestClient(t, srv.URL))
	assert.NoError(t, errs["RemoveCartItem"], "a 204 names no cart")
	delete(errs, "RemoveCartItem")
	for name, err := range errs {
		assert.ErrorIs(t, err, ErrCartUnreachable, name)
	}
}

// Django answers an unknown, expired or revoked token with 401 and no
// cart. Every call reports it as refused (an unreachable cart, never a
// re-link prompt), the token records it for the turn's done event, and
// later calls of the turn do not ask again.
func TestRefusedCartTokenIsReportedAndNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(
				`{"detail": "The cart token is invalid or has expired."}`))
		}))
	defer srv.Close()
	ctx, tok := tokenContext(t)

	for name, err := range cartCalls(ctx, newTestClient(t, srv.URL)) {
		assert.ErrorIs(t, err, ErrCartTokenRefused, name)
		assert.ErrorIs(t, err, ErrCartUnreachable, name)
		assert.NotErrorIs(t, err, ErrUnauthorized, name)
	}
	assert.True(t, tok.Refused())
	assert.Equal(t, int32(1), calls.Load())
}

// A 401 without a token keeps its meaning: a missing or bad credential.
func TestUnauthorizedWithoutATokenIsNotARefusedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).GetCart(context.Background(),
		"shop.example.test", "el", fixtureCartID)
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.NotErrorIs(t, err, ErrCartTokenRefused)
}

// Stock, order and payment calls never carry the token: Django refuses
// it there, and only the cart routes may act on a signed-in cart.
func TestCheckoutCallsNeverSendTheToken(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, sent := r.Header["X-Cart-Token"]
			assert.False(t, sent, r.Method+" "+r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}))
	defer srv.Close()
	ctx, _ := tokenContext(t)
	c := newTestClient(t, srv.URL)

	_, _ = c.ReserveStock(ctx, "shop.example.test", "el", fixtureCartID)
	_, _ = c.CreateOrder(ctx, "shop.example.test", "el", fixtureCartID,
		OrderCreate{})
	assert.Equal(t, int32(2), calls.Load())
}

func TestParseCartTokenChecksTheShape(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":         "",
		"too long":      strings.Repeat("a", maxCartTokenLen+1),
		"header break":  "abc\r\nX-Cart-Id: 1",
		"not url-safe":  "abc+def/ghi=",
		"spaces inside": "abc def",
	} {
		_, err := ParseCartToken(raw)
		assert.ErrorIs(t, err, ErrInvalidCartToken, name)
	}
	_, err := ParseCartToken(testCartToken)
	assert.NoError(t, err)
}

// The token is a credential: printing or logging it shows nothing.
func TestCartTokenNeverPrintsItsValue(t *testing.T) {
	_, tok := tokenContext(t)
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("turn", slog.Any("token", tok), "raw", tok)
	out := buf.String() + fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok)

	assert.NotContains(t, out, testCartToken)
	assert.Contains(t, out, "[redacted]")
}
