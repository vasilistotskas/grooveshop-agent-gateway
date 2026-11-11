package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// errorEvent ends a failed turn. Code is set for the failures the widget
// words itself: codeRateLimited, with RetryAfter (seconds) when the model
// provider said how long to wait.
type errorEvent struct {
	Message    string `json:"message"`
	Code       string `json:"code,omitempty"`
	RetryAfter int    `json:"retryAfter,omitempty"`
}

const codeRateLimited = "rate_limited"

type doneEvent struct {
	ConversationID string `json:"conversationId"`
	CartID         string `json:"cartId,omitempty"`
	CartMutated    bool   `json:"cartMutated"`
	// CartTokenRefused: Django refused the turn's cart token, so the
	// shopper's cart was not reached; the widget mints a new one.
	CartTokenRefused bool `json:"cartTokenRefused"`
}

type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (s *sseWriter) event(name string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw)
	s.f.Flush()
}
