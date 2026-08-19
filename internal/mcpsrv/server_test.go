package mcpsrv

import (
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

	acme := NewServer(deps, "Acme Store")
	require.NotNil(t, acme)

	other := NewServer(deps, "Aurora Store")
	require.NotNil(t, other)

	// Distinct instances so their advertised titles cannot collide.
	assert.NotSame(t, acme, other)
}

func TestServerCacheReusesPerSchema(t *testing.T) {
	c := &serverCache{deps: Deps{Version: "test"}, servers: map[string]*mcp.Server{}}

	first := c.get("acme", "Acme Store")
	again := c.get("acme", "Acme Store")
	assert.Same(t, first, again, "same schema must reuse its server")

	other := c.get("aurora", "Aurora Store")
	assert.NotSame(t, first, other, "different schema gets its own server")
}

func TestServerCacheIsBounded(t *testing.T) {
	c := &serverCache{deps: Deps{Version: "test"}, servers: map[string]*mcp.Server{}}
	for i := 0; i < maxCachedServers+5; i++ {
		c.get(fmt.Sprintf("tenant_%d", i), "Store")
	}
	assert.LessOrEqual(t, len(c.servers), maxCachedServers)
}

func TestServerTitleFallsBackWhenBlank(t *testing.T) {
	// A tenant with neither store name nor name must still advertise
	// something sane rather than an empty title.
	assert.NotNil(t, NewServer(Deps{Version: "test"}, ""))
}
