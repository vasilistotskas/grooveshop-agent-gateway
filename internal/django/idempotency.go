package django

import "context"

type idempotencyKey struct{}

// WithIdempotencyKey has the mutation sent with ctx carry key as
// Idempotency-Key. Django's middleware then answers a repeat of it with
// the first outcome instead of acting twice (24h, per caller scope; a
// scope past its key budget is served without the guard). Use it on one
// call's ctx: every mutation made with ctx sends the same key.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idempotencyKey{}, key)
}

func idempotencyKeyFrom(ctx context.Context) string {
	key, _ := ctx.Value(idempotencyKey{}).(string)
	return key
}
