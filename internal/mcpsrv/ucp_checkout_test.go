package mcpsrv

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// A checkout store that cannot be reached is the store being unavailable,
// told as a ToolError like any other outage.
func TestCheckoutStoreOutageIsUnavailable(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 20 * time.Millisecond,
		MaxRetries: -1,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	h := &handlers{deps: Deps{Checkout: checkout.NewStore(rdb)}}

	_, _, err := h.lockedSession(t.Context(), cartTenant(), uuid.NewString())

	var got *ToolError
	require.ErrorAs(t, err, &got)
	assert.Equal(t, "unavailable", got.Code)
	assert.NotEmpty(t, got.Hint)
}
