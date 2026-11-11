//go:build integration

package integration

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A checkout the gateway stores no session for may never have been
// completed, or may have placed an order and no longer be kept (it
// expired, Redis evicted it, a release changed the session key), or its id may be
// wrong. The id does not say which, so no tool and no route answers
// only "start a new one": the caller is told an order may exist, and
// how to check.
func TestAMissingCheckoutMayHavePlacedAnOrder(t *testing.T) {
	stack := startUCPGateway(t)
	id := uuid.NewString()

	t.Run("acp", func(t *testing.T) {
		base := stack.gw.URL + "/acp/checkout_sessions/" + id
		calls := map[string]*http.Request{
			"get": acpRequest(t, http.MethodGet, base, nil, ""),
			"update": acpRequest(t, http.MethodPost, base,
				map[string]any{}, uuid.NewString()),
			"complete": acpRequest(t, http.MethodPost, base+"/complete",
				map[string]any{"payment_data": map[string]any{}},
				uuid.NewString()),
			"cancel": acpRequest(t, http.MethodPost, base+"/cancel", nil,
				uuid.NewString()),
		}
		for name, req := range calls {
			resp, body := acpDo(t, req)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, name)
			assert.Equal(t, "not_found", body["code"], name)
			assert.Contains(t, body["message"], "an order may exist", name)
			assert.Contains(t, body["message"],
				"before creating another session", name)
		}
	})

	t.Run("ucp", func(t *testing.T) {
		session := connectMCP(t, stack.gw.URL)
		platform := httptest.NewServer(newWebhookSink().handler())
		t.Cleanup(platform.Close)
		profile := servePlatformProfile(t, platform.URL+"/ucp/orders", nil)
		meta := func(idem string) map[string]any {
			m := map[string]any{
				"ucp-agent": map[string]any{"profile": profile},
			}
			if idem != "" {
				m["idempotency-key"] = idem
			}
			return m
		}
		calls := map[string]map[string]any{
			"get_checkout": {"meta": meta(""), "id": id},
			"update_checkout": {
				"meta": meta(""), "id": id, "checkout": map[string]any{},
			},
			"complete_checkout": {
				"meta": meta(uuid.NewString()), "id": id,
				"checkout": map[string]any{},
			},
			"cancel_checkout": {"meta": meta(uuid.NewString()), "id": id},
		}
		for tool, args := range calls {
			res := callTool(t, session, tool, args)
			require.True(t, res.IsError, tool)
			text := res.Content[0].(*mcp.TextContent).Text
			assert.Contains(t, text, "an order may exist", tool)
			assert.Contains(t, text,
				"do not start another checkout before checking", tool)
			assert.Contains(t, text, "get_order or track_order", tool)
			assert.Contains(t, text,
				"ask the shopper for the store's order confirmation", tool)
		}

		// get_order for an order no checkout link is held for: it was
		// placed elsewhere, or here and its link is gone (expired, or
		// evicted). The answer is true in both, and names the tool that
		// reads the order from the store.
		res := callTool(t, session, "get_order", map[string]any{
			"meta": meta(""), "id": fixtureOrderUUID,
		})
		require.True(t, res.IsError)
		text := res.Content[0].(*mcp.TextContent).Text
		assert.Contains(t, text, "order "+fixtureOrderUUID+" exists")
		assert.Contains(t, text,
			"or it was and its checkout record is no longer kept")
		assert.Contains(t, text, "track_order")
	})
}
