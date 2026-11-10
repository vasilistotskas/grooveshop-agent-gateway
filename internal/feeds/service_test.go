package feeds

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// A panic in a generation comes back as an error, without the panic
// text or stack, instead of ending the process from singleflight's own
// goroutine.
func TestRefreshReturnsPanicAsError(t *testing.T) {
	// No Redis client: reading the epoch panics.
	s := &Service{log: slog.New(slog.DiscardHandler)}
	res := <-s.refresh(&tenant.Tenant{
		TenantConfig: django.TenantConfig{SchemaName: "demostore"},
	})
	require.EqualError(t, res.Err, "feeds: generation: panicked")
}
