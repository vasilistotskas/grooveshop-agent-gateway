package django

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// A transport failure names the URL, and a staff search carries what was
// looked up: a customer's email must reach neither the log nor the error.
func TestATransportFailureLeavesTheQueryOut(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	var logged bytes.Buffer
	c := New("http://"+addr+"/api/v1", "api.example.test", "secret",
		2*time.Second, slog.New(slog.NewTextHandler(&logged, nil)),
		obs.NewMetrics())

	_, err = c.StaffCustomers(context.Background(), "shop.example.test",
		"el", "token", "jane@example.gr", 5)

	require.ErrorIs(t, err, ErrUpstreamDown)
	assert.NotContains(t, err.Error(), "jane")
	assert.NotContains(t, logged.String(), "jane")
	assert.Contains(t, err.Error(), "/agent/staff/customers")
}
