package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// turnCart is what a turn's last event tells the widget about the cart.
// A turn that failed says it like one that finished: the store keeps a
// change it accepted whatever became of the turn.
type turnCart struct {
	// CartID is the visitor's cart, for a widget that sent none to
	// adopt. A signed-in shopper's turn names none: their cart is their
	// session's.
	CartID string `json:"cartId,omitempty"`
	// CartMutated: the store accepted a cart change this turn.
	CartMutated bool `json:"cartMutated"`
	// CartTokenRefused: Django refused the turn's cart token, so the
	// shopper's cart was not reached; the widget mints a new one.
	CartTokenRefused bool `json:"cartTokenRefused"`
}

// errorEvent ends a failed turn. Code is set for the failures the widget
// words itself: codeRateLimited, with RetryAfter (seconds) when the model
// provider said how long to wait. ConversationID is set when the failed
// turn stored its conversation (Service.endFailed), for the widget to go
// on with.
type errorEvent struct {
	Message        string `json:"message"`
	Code           string `json:"code,omitempty"`
	RetryAfter     int    `json:"retryAfter,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	turnCart
}

const codeRateLimited = "rate_limited"

type doneEvent struct {
	ConversationID string `json:"conversationId"`
	turnCart
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
