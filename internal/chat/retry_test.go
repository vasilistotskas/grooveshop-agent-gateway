package chat

import (
	"net/http"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
)

// The provider's wait reaches the widget in whole seconds, rounded up:
// its Retry-After header first, else Gemini's hint in the body.
func TestModelRetryAfter(t *testing.T) {
	header := &openai.Error{Response: &http.Response{
		Header: http.Header{"Retry-After": {"20"}}}}
	assert.Equal(t, 20, modelRetryAfter(header, "retry in 3s"))

	hinted := &openai.Error{Response: &http.Response{Header: http.Header{}}}
	assert.Equal(t, 13, modelRetryAfter(hinted,
		`[{"error": {"message": "Quota exceeded. Please retry in 12.3s."}}]`))

	assert.Equal(t, 0, modelRetryAfter(&openai.Error{}, "quota exceeded"))
}

func TestSecondsRoundsUp(t *testing.T) {
	assert.Equal(t, 1, seconds(10*time.Millisecond))
	assert.Equal(t, 60, seconds(time.Minute))
	assert.Equal(t, 61, seconds(time.Minute+time.Millisecond))
}
