package feeds

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// A panic in a generation comes back as an error instead of ending the
// process from singleflight's own goroutine.
func TestRefreshReturnsPanicAsError(t *testing.T) {
	s := &Service{} // no Redis client: reading the epoch panics
	res := <-s.refresh(&tenant.Tenant{
		TenantConfig: django.TenantConfig{SchemaName: "demostore"},
	})
	require.ErrorContains(t, res.Err, "feeds: generation panicked")
}
