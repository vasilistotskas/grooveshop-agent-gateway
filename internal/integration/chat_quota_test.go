//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
)

// postChatFrom sends a turn as the visitor Traefik would name in
// X-Forwarded-For and returns the status plus the error body, if any.
func postChatFrom(
	t *testing.T, gwURL, visitor string, body ...map[string]any,
) (int, string) {
	t.Helper()
	payload := map[string]any{"message": "γεια"}
	if len(body) > 0 {
		payload = body[0]
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, gwURL+"/chat",
		bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", visitor)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var refusal map[string]string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&refusal))
		return resp.StatusCode, refusal["error"]
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, ""
}

// The store's hourly budget is shared by every visitor: once it is spent,
// a fresh address is refused too, before any model call.
func TestChatStoreHourlyBudgetHoldsAcrossVisitors(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		textTurnSSE("ένα"), textTurnSSE("δύο"),
	}}
	gw := startChatGateway(t, fake, func(c *config.Config) {
		c.ChatStoreTurnsPerHour = 2
	})

	status, _ := postChatFrom(t, gw.URL, "203.0.113.1")
	assert.Equal(t, http.StatusOK, status)
	status, _ = postChatFrom(t, gw.URL, "203.0.113.2")
	assert.Equal(t, http.StatusOK, status)

	status, message := postChatFrom(t, gw.URL, "203.0.113.3")
	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Contains(t, message, "βοηθός", "refusal speaks the store's language")
	assert.EqualValues(t, 2, fake.calls.Load(), "no model call once spent")
}

// A visitor's daily cap is theirs alone, and a refused turn spends
// nothing: the locked-out visitor does not drain the store's budget.
func TestChatVisitorDailyCapIsPerVisitorAndRefusalsAreFree(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		textTurnSSE("ένα"), textTurnSSE("δύο"),
	}}
	gw := startChatGateway(t, fake, func(c *config.Config) {
		c.ChatVisitorTurnsPerDay = 1
		c.ChatStoreTurnsPerHour = 2
	})

	status, _ := postChatFrom(t, gw.URL, "198.51.100.7")
	assert.Equal(t, http.StatusOK, status)
	for range 3 {
		status, _ = postChatFrom(t, gw.URL, "198.51.100.7")
		assert.Equal(t, http.StatusTooManyRequests, status)
	}

	// Had the refusals counted against the store, its budget of 2 would
	// be gone and this visitor's first turn refused.
	status, _ = postChatFrom(t, gw.URL, "198.51.100.8")
	assert.Equal(t, http.StatusOK, status)
	assert.EqualValues(t, 2, fake.calls.Load())
}

// A request rejected for its conversation (a forged id) spends nothing,
// so it cannot be used to lock a visitor or a store out.
func TestChatRejectedConversationSpendsNoQuota(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{textTurnSSE("ένα")}}
	gw := startChatGateway(t, fake, func(c *config.Config) {
		c.ChatVisitorTurnsPerDay = 1
		c.ChatStoreTurnsPerHour = 1
	})

	for range 3 {
		status, _ := postChatFrom(t, gw.URL, "192.0.2.4", map[string]any{
			"message": "γεια", "conversationId": "../evil",
		})
		assert.Equal(t, http.StatusBadRequest, status)
	}
	status, _ := postChatFrom(t, gw.URL, "192.0.2.4")
	assert.Equal(t, http.StatusOK, status)
	assert.EqualValues(t, 1, fake.calls.Load())
}
