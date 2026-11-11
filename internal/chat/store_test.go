package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadRejectsMalformedConversationID(t *testing.T) {
	s := NewStore(nil, 0, 10)
	_, err := s.Load(t.Context(), "demostore", "../evil")
	assert.ErrorIs(t, err, ErrInvalidConversation,
		"client input, answered 400 rather than 503")
}

func TestLoadEmptyIDStartsFresh(t *testing.T) {
	s := NewStore(nil, 0, 10)
	c, err := s.Load(t.Context(), "demostore", "")
	assert.NoError(t, err)
	assert.NotEmpty(t, c.ID)
	assert.Empty(t, c.Turns)
}
