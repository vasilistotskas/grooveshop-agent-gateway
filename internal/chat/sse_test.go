package chat

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
)

const testCartID = "0b7d5a52-8f6e-4c1d-9d2a-5e4f3c2b1a09"

// sent is the one event written to rec: its name and its data.
func sent(t *testing.T, rec *httptest.ResponseRecorder) (name, data string) {
	t.Helper()
	frame, ok := strings.CutSuffix(rec.Body.String(), "\n\n")
	require.True(t, ok, "an event ends with a blank line: %q", rec.Body)
	head, data, ok := strings.Cut(frame, "\ndata: ")
	require.True(t, ok, frame)
	require.NotContains(t, data, "\n", "one event")
	return strings.TrimPrefix(head, "event: "), data
}

func eventOf(t *testing.T, name string, v any) string {
	t.Helper()
	rec := httptest.NewRecorder()
	(&sseWriter{w: rec, f: rec}).event(name, v)
	got, data := sent(t, rec)
	require.Equal(t, name, got)
	return data
}

// The widget reads these members by name (useShopChat.ts). done's are
// unchanged; error carries done's cart members beside its own, and a
// widget that reads only message, code and retryAfter ignores the rest.
func TestTurnEndEventsCarryTheCartMembers(t *testing.T) {
	cart := turnCart{CartID: testCartID, CartMutated: true}

	assert.Equal(t, `{"conversationId":"c-1","cartId":"`+testCartID+
		`","cartMutated":true,"cartTokenRefused":false}`,
		eventOf(t, "done", doneEvent{ConversationID: "c-1", turnCart: cart}))
	assert.Equal(t, `{"message":"m","code":"rate_limited","retryAfter":3,`+
		`"conversationId":"c-1","cartId":"`+testCartID+
		`","cartMutated":true,"cartTokenRefused":false}`,
		eventOf(t, "error", errorEvent{Message: "m", Code: codeRateLimited,
			RetryAfter: 3, ConversationID: "c-1", turnCart: cart}))
	// A turn that changed nothing still says so; what it has no value
	// for is left out.
	assert.Equal(t,
		`{"message":"m","cartMutated":false,"cartTokenRefused":false}`,
		eventOf(t, "error", errorEvent{Message: "m"}))
	assert.Equal(t,
		`{"message":"m","cartMutated":true,"cartTokenRefused":true}`,
		eventOf(t, "error", errorEvent{Message: "m", turnCart: turnCart{
			CartMutated: true, CartTokenRefused: true}}))
}

// However a turn fails, whoever still reads the stream is told what its
// tools did to the cart: on a provider failure and when the pod stops. A
// shopper who left reads nothing, so nothing is written.
func TestEndFailedSaysWhatTheTurnDidToTheCart(t *testing.T) {
	cart := turnCart{CartID: testCartID, CartMutated: true}
	want := `{"message":"The conversation was interrupted for a moment — ` +
		`please try again.","cartId":"` + testCartID +
		`","cartMutated":true,"cartTokenRefused":false}`
	ended := func(cause error) context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		return ctx
	}
	s := &Service{log: slog.New(slog.DiscardHandler)}
	end := func(ctx context.Context) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.endFailed(ctx, testTenant(), "en", &Conversation{ID: "c-1"},
			&sseWriter{w: rec, f: rec}, cart, false, false,
			errors.New("model down"))
		return rec
	}

	for name, ctx := range map[string]context.Context{
		"the provider failed": context.Background(),
		"the pod is stopping": ended(httpmw.ErrShuttingDown),
	} {
		t.Run(name, func(t *testing.T) {
			event, data := sent(t, end(ctx))
			assert.Equal(t, "error", event)
			assert.Equal(t, want, data)
		})
	}
	assert.Empty(t, end(ended(nil)).Body.String(), "the shopper left")
}
