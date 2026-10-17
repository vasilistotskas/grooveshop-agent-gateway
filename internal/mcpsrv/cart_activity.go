package mcpsrv

import (
	"context"
	"sync/atomic"
)

// CartActivity records whether a chat turn's cart tools changed the
// cart. The tools record it themselves, the moment Django accepts the
// change: a tool that then fails to read the cart back still changed
// it, and the widget must refresh rather than be told nothing happened.
type CartActivity struct {
	mutated atomic.Bool
}

func (a *CartActivity) Mutated() bool { return a.mutated.Load() }

type cartActivityKey struct{}

// WithCartActivity has the cart tools called with ctx record into a.
func WithCartActivity(ctx context.Context, a *CartActivity) context.Context {
	return context.WithValue(ctx, cartActivityKey{}, a)
}

// cartChanged records an accepted cart change, when a turn listens.
func cartChanged(ctx context.Context) {
	if a, ok := ctx.Value(cartActivityKey{}).(*CartActivity); ok {
		a.mutated.Store(true)
	}
}
