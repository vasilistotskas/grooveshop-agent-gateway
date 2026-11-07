//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// A caller touching an unpaid placed checkout must not keep it alive: it reads
// its order past the limit on a guest's uuid.
func TestPlacedSessionCannotBeKeptAlive(t *testing.T) {
	ctx := context.Background()
	rdb := startRedis(t)
	store := checkout.NewStore(rdb)
	key := func(s *checkout.Session) string {
		return "ag:" + s.Schema + ":cs:" + s.ID
	}

	s := checkout.NewSession("demostore", "shop.test", checkout.ProtocolUCP, "cart")
	s.Status = checkout.StatusRequiresEscalation
	s.PlacedAt = time.Now().Add(-24*time.Hour + 5*time.Minute)
	require.NoError(t, store.Save(ctx, s))
	ttl, err := rdb.TTL(ctx, key(s)).Result()
	require.NoError(t, err)
	assert.Positive(t, ttl)
	assert.LessOrEqual(t, ttl, 5*time.Minute)

	s.PlacedAt = time.Now().Add(-24 * time.Hour)
	require.NoError(t, store.Save(ctx, s))
	_, err = store.Load(ctx, s.Schema, s.ID)
	assert.ErrorIs(t, err, checkout.ErrNotFound)
}
