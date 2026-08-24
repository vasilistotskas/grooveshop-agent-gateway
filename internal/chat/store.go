// Package chat implements the first-party shopping assistant: an SSE
// endpoint that loops an OpenAI-compatible model (openai-go; the Gemini
// free tier by default, any provider via CHAT_BASE_URL) over the gateway's
// own commerce tools via an in-process MCP client — one tool surface for
// external agents and the storefront widget alike.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Turn is one persisted conversation exchange. Only text is stored:
// tool-use blocks are transient within a turn and the assistant's prose
// carries the durable context ("I added it to your cart").
type Turn struct {
	Role string `json:"role"` // "user" or "assistant"
	Text string `json:"text"`
}

type Conversation struct {
	ID        string    `json:"id"`
	CartID    string    `json:"cartId,omitempty"`
	Turns     []Turn    `json:"turns"`
	CreatedAt time.Time `json:"createdAt"`
}

// ErrConversationFull marks a conversation that reached its turn cap.
var ErrConversationFull = errors.New("chat: conversation turn cap reached")

// Store persists conversations in Redis, tenant-prefixed. Chat fails
// closed without Redis — no in-memory fallback — because a conversation
// that silently loses history mid-shopping is worse than a clean error.
type Store struct {
	rdb      *redis.Client
	ttl      time.Duration
	maxTurns int
}

func NewStore(rdb *redis.Client, ttl time.Duration, maxTurns int) *Store {
	return &Store{rdb: rdb, ttl: ttl, maxTurns: maxTurns}
}

func key(schema, id string) string {
	return "ag:" + schema + ":chat:" + id
}

// Load fetches a conversation, or starts a fresh one when id is empty or
// unknown/expired (an expired chat restarting cleanly is the desired UX).
func (s *Store) Load(
	ctx context.Context, schema, id string,
) (*Conversation, error) {
	if id == "" {
		return newConversation(), nil
	}
	if err := uuid.Validate(id); err != nil {
		return nil, fmt.Errorf("chat: invalid conversation id")
	}
	raw, err := s.rdb.Get(ctx, key(schema, id)).Result()
	if errors.Is(err, redis.Nil) {
		return newConversation(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("chat: load conversation: %w", err)
	}
	var c Conversation
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("chat: corrupt conversation: %w", err)
	}
	if len(c.Turns) >= s.maxTurns {
		return nil, ErrConversationFull
	}
	return &c, nil
}

func (s *Store) Save(ctx context.Context, schema string, c *Conversation) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("chat: encode conversation: %w", err)
	}
	if err := s.rdb.Set(ctx, key(schema, c.ID), raw, s.ttl).Err(); err != nil {
		return fmt.Errorf("chat: save conversation: %w", err)
	}
	return nil
}

func newConversation() *Conversation {
	return &Conversation{
		ID:        uuid.NewString(),
		CreatedAt: time.Now().UTC(),
	}
}
