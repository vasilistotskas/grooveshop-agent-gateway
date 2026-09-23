package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The MCP server tells an agent who it is talking to in the initialize
// response. On a white-label storefront that has to be the MERCHANT:
// the neighbouring surfaces (/.well-known/ucp, the feeds) already
// resolve the merchant name, and a hardcoded platform title made every
// tenant's endpoint introduce itself as the platform.
func TestServerTitleIsPerTenant(t *testing.T) {
	deps := Deps{Version: "test"}

	acme := NewServer(deps, Identity{Name: "test.acme/store", Title: "Acme Store"})
	require.NotNil(t, acme)

	other := NewServer(deps, Identity{Name: "test.aurora/store", Title: "Aurora Store"})
	require.NotNil(t, other)

	// Distinct instances so their advertised titles cannot collide.
	assert.NotSame(t, acme, other)
}

// A stateless server has no session to send list-changed notifications
// on, and logging is deprecated as of 2026-07-28: advertise tools only.
func TestServerAdvertisesOnlyWhatItHonours(t *testing.T) {
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	_, err := NewServer(Deps{Version: "test"},
		Identity{Name: "test.acme/store", Title: "Acme Store"}).
		Connect(ctx, serverT, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(ctx, clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	raw, err := json.Marshal(session.InitializeResult().Capabilities)
	require.NoError(t, err)
	assert.JSONEq(t, `{"tools":{}}`, string(raw))
}

// A store rename must reach initialize without a restart.
func TestServerCacheFollowsTheStoreTitle(t *testing.T) {
	c := &serverCache{deps: Deps{Version: "test"}, servers: map[string]*mcp.Server{}}
	before := c.get("acme", Identity{Name: "test.acme/store", Title: "Acme Store"})
	assert.NotSame(t, before, c.get("acme", Identity{Name: "test.acme/store", Title: "Acme Outlet"}))
}

func TestServerCacheReusesPerSchema(t *testing.T) {
	c := &serverCache{deps: Deps{Version: "test"}, servers: map[string]*mcp.Server{}}

	first := c.get("acme", Identity{Name: "test.acme/store", Title: "Acme Store"})
	again := c.get("acme", Identity{Name: "test.acme/store", Title: "Acme Store"})
	assert.Same(t, first, again, "same schema must reuse its server")

	other := c.get("aurora", Identity{Name: "test.aurora/store", Title: "Aurora Store"})
	assert.NotSame(t, first, other, "different schema gets its own server")
}

func TestServerCacheIsBounded(t *testing.T) {
	c := &serverCache{deps: Deps{Version: "test"}, servers: map[string]*mcp.Server{}}
	for i := range maxCachedServers + 5 {
		c.get(fmt.Sprintf("tenant_%d", i), Identity{Name: "test/store", Title: "Store"})
	}
	assert.LessOrEqual(t, len(c.servers), maxCachedServers)
}
