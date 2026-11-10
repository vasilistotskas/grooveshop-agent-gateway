package chat

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The provider's wait reaches the widget in whole seconds, rounded up:
// its Retry-After header first, else Gemini's hint in the body.
func TestModelRetryAfter(t *testing.T) {
	header := providerError{
		header: http.Header{"Retry-After": {"20"}}, body: "retry in 3s"}
	assert.Equal(t, 20, header.retryAfter())

	hinted := providerError{header: http.Header{},
		body: `[{"error": {"message": "Quota exceeded. ` +
			`Please retry in 12.3s."}}]`}
	assert.Equal(t, 13, hinted.retryAfter())

	assert.Equal(t, 0, providerError{body: "quota exceeded"}.retryAfter())
}

// A provider error is read the same way whether the provider answered the
// request with it or sent it inside the stream; anything else is none.
func TestProviderErrorOf(t *testing.T) {
	answered, ok := providerErrorOf(fmt.Errorf("chat: %w", &openai.Error{
		StatusCode: http.StatusTooManyRequests,
		Request:    &http.Request{Header: http.Header{}},
		Response: &http.Response{
			Header: http.Header{"Retry-After": {"4"}}},
	}))
	require.True(t, ok)
	assert.Equal(t, http.StatusTooManyRequests, answered.status)
	assert.Equal(t, 4, answered.retryAfter())

	streamed, ok := providerErrorOf(&ssestream.StreamError{
		Event: ssestream.Event{Data: []byte(`{"error": {"code": 429, ` +
			`"message": "Rate limit exceeded"}}`)},
	})
	require.True(t, ok)
	assert.Equal(t, http.StatusTooManyRequests, streamed.status)

	quoted, ok := providerErrorOf(&ssestream.StreamError{
		Event: ssestream.Event{Data: []byte(`{"error": {"code": "429"}}`)},
	})
	require.True(t, ok)
	assert.Equal(t, http.StatusTooManyRequests, quoted.status)

	named, ok := providerErrorOf(&ssestream.StreamError{
		Event: ssestream.Event{Data: []byte(
			`{"error": {"code": "server_error"}}`)},
	})
	require.True(t, ok)
	assert.Zero(t, named.status, "a code that is no status is none")

	_, ok = providerErrorOf(fmt.Errorf("chat: %w", http.ErrHandlerTimeout))
	assert.False(t, ok)
}

func TestSecondsRoundsUp(t *testing.T) {
	assert.Equal(t, 1, seconds(10*time.Millisecond))
	assert.Equal(t, 60, seconds(time.Minute))
	assert.Equal(t, 61, seconds(time.Minute+time.Millisecond))
}
