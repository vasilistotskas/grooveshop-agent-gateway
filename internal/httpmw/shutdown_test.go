package httpmw

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A context that lost its cancellation still ends at the server's cut.
func TestUntilShutdownReachesADetachedContext(t *testing.T) {
	requests, cut := context.WithCancelCause(context.Background())
	detached := context.WithoutCancel(WithShutdown(requests))

	ctx, stop := UntilShutdown(detached)
	defer stop()
	assert.NoError(t, ctx.Err())

	cut(ErrShuttingDown)

	<-ctx.Done()
	assert.ErrorIs(t, context.Cause(ctx), ErrShuttingDown)
}

// A context that came through no server is left as it is.
func TestUntilShutdownWithoutAServer(t *testing.T) {
	ctx := context.Background()

	got, stop := UntilShutdown(ctx)
	defer stop()

	assert.Equal(t, ctx, got)
}

// A call that starts after the cut is cut at once, not a moment later.
func TestUntilShutdownAfterTheCut(t *testing.T) {
	requests, cut := context.WithCancelCause(context.Background())
	cut(ErrShuttingDown)

	ctx, stop := UntilShutdown(context.WithoutCancel(WithShutdown(requests)))
	defer stop()

	assert.ErrorIs(t, context.Cause(ctx), ErrShuttingDown)
}
