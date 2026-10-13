package chat

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcpsrv"
)

// offeredNames lists the tools the bridge offers the model for a store
// with features, over the shared server that carries every tool.
func offeredNames(t *testing.T, features django.StoreFeatures) []string {
	t.Helper()
	server := mcpsrv.NewServer(mcpsrv.Deps{Version: "test"},
		mcpsrv.BridgeIdentity, mcpsrv.EveryFeature)
	br, err := newBridge(context.Background(), server)
	require.NoError(t, err)
	t.Cleanup(br.close)
	tools, err := br.tools(context.Background(), features)
	require.NoError(t, err)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.GetFunction().Name)
	}
	return names
}

// The bridge shares one server across stores, so it leaves out a tool
// whose feature the store switched off — and only an offered tool is
// callable, so the model cannot reach it either.
func TestBridgeOffersOnlyTheStoreTools(t *testing.T) {
	every := offeredNames(t, mcpsrv.EveryFeature)
	assert.Contains(t, every, "get_product_reviews")

	noReviews := mcpsrv.EveryFeature
	noReviews.ProductReviews = false
	offered := offeredNames(t, noReviews)
	assert.NotContains(t, offered, "get_product_reviews")
	assert.Len(t, offered, len(every)-1)
}
