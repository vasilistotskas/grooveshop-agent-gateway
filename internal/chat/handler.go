package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// refusalMessage is shown when safety classifiers decline a request; the
// widget audience is Greek-first.
const refusalMessage = "Λυπάμαι, δεν μπορώ να βοηθήσω με αυτό το αίτημα. " +
	"Μπορώ όμως να σε βοηθήσω να βρεις προϊόντα, να δεις διαθεσιμότητα " +
	"και να ολοκληρώσεις την παραγγελία σου!"

type Service struct {
	cfg    config.Config
	server *mcp.Server
	store  *Store
	client anthropic.Client
	log    *slog.Logger
}

// New builds the chat service. Extra options are passed to the Anthropic
// client (tests inject option.WithBaseURL for a fake API).
func New(
	cfg config.Config, server *mcp.Server, store *Store, log *slog.Logger,
	opts ...option.RequestOption,
) *Service {
	clientOpts := append(
		[]option.RequestOption{option.WithAPIKey(cfg.AnthropicAPIKey)},
		opts...,
	)
	return &Service{
		cfg:    cfg,
		server: server,
		store:  store,
		client: anthropic.NewClient(clientOpts...),
		log:    log,
	}
}

// Enabled reports whether the chat surface is configured; without an API
// key the route is not mounted at all.
func (s *Service) Enabled() bool { return s.cfg.AnthropicAPIKey != "" }

type chatRequest struct {
	ConversationID string `json:"conversationId"`
	Message        string `json:"message"`
	CartID         string `json:"cartId"`
}

type doneEvent struct {
	ConversationID string `json:"conversationId"`
	CartID         string `json:"cartId,omitempty"`
	CartMutated    bool   `json:"cartMutated"`
}

func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(s.handle)
}

func (s *Service) handle(w http.ResponseWriter, r *http.Request) {
	t, ok := tenant.FromContext(r.Context())
	if !ok {
		http.Error(w, "unknown store", http.StatusNotFound)
		return
	}

	var req chatRequest
	if err := json.NewDecoder(
		http.MaxBytesReader(w, r.Body, 64<<10),
	).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		jsonError(w, http.StatusBadRequest, "message is required")
		return
	}
	if len(req.Message) > s.cfg.ChatMaxMessageLen {
		jsonError(w, http.StatusBadRequest, "message is too long")
		return
	}

	conv, err := s.store.Load(r.Context(), t.SchemaName, req.ConversationID)
	switch {
	case errors.Is(err, ErrConversationFull):
		jsonError(w, http.StatusConflict,
			"this conversation is finished; start a new one")
		return
	case err != nil:
		s.log.ErrorContext(r.Context(), "chat load failed",
			slog.String("error", err.Error()))
		jsonError(w, http.StatusServiceUnavailable,
			"chat is temporarily unavailable")
		return
	}
	// The widget's session cart wins: the bot must operate on the cart the
	// shopper sees in the UI.
	if req.CartID != "" {
		conv.CartID = req.CartID
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	sse := &sseWriter{w: w, f: flusher}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	assistantText, cartID, cartMutated, err := s.runTurn(r, t, conv, req.Message, sse)
	if err != nil {
		s.log.ErrorContext(r.Context(), "chat turn failed",
			slog.String("tenant", t.SchemaName),
			slog.String("conversation", conv.ID),
			slog.String("error", err.Error()),
		)
		sse.event("error", map[string]string{
			"message": "Η συνομιλία διακόπηκε προσωρινά — δοκίμασε ξανά.",
		})
		return
	}

	conv.Turns = append(conv.Turns,
		Turn{Role: "user", Text: req.Message},
		Turn{Role: "assistant", Text: assistantText},
	)
	if cartID != "" {
		conv.CartID = cartID
	}
	if err := s.store.Save(r.Context(), t.SchemaName, conv); err != nil {
		s.log.ErrorContext(r.Context(), "chat save failed",
			slog.String("error", err.Error()))
	}

	sse.event("done", doneEvent{
		ConversationID: conv.ID,
		CartID:         conv.CartID,
		CartMutated:    cartMutated,
	})
}

// runTurn executes one assistant turn: Claude looping over the commerce
// tools via the in-process MCP bridge, streaming text deltas to the client.
func (s *Service) runTurn(
	r *http.Request,
	t *tenant.Tenant,
	conv *Conversation,
	userMessage string,
	sse *sseWriter,
) (assistantText, cartID string, cartMutated bool, err error) {
	ctx := r.Context()

	br, err := newBridge(ctx, s.server)
	if err != nil {
		return "", "", false, fmt.Errorf("chat: bridge: %w", err)
	}
	defer br.close()

	tools, err := br.tools(ctx)
	if err != nil {
		return "", "", false, fmt.Errorf("chat: list tools: %w", err)
	}

	messages := make([]anthropic.BetaMessageParam, 0, len(conv.Turns)+1)
	for _, turn := range conv.Turns {
		role := anthropic.BetaMessageParamRoleUser
		if turn.Role == "assistant" {
			role = anthropic.BetaMessageParamRoleAssistant
		}
		messages = append(messages, anthropic.BetaMessageParam{
			Role: role,
			Content: []anthropic.BetaContentBlockParamUnion{{
				OfText: &anthropic.BetaTextBlockParam{Text: turn.Text},
			}},
		})
	}
	messages = append(messages,
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(userMessage)))

	runner := s.client.Beta.Messages.NewToolRunnerStreaming(tools,
		anthropic.BetaToolRunnerParams{
			BetaMessageNewParams: anthropic.BetaMessageNewParams{
				Model:     anthropic.Model(s.cfg.ChatModel),
				MaxTokens: int64(s.cfg.ChatMaxTokens),
				System: []anthropic.BetaTextBlockParam{
					{Text: systemPrompt(t, conv.CartID)},
				},
				Messages: messages,
				OutputConfig: anthropic.BetaOutputConfigParam{
					Effort: anthropic.BetaOutputConfigEffort(s.cfg.ChatEffort),
				},
			},
			MaxIterations: s.cfg.ChatMaxIterations,
		})

	var text strings.Builder
	for stream, err := range runner.AllStreaming(ctx) {
		if err != nil {
			return "", "", false, err
		}
		for event, err := range stream {
			if err != nil {
				return "", "", false, err
			}
			if delta, ok := event.AsAny().(anthropic.BetaRawContentBlockDeltaEvent); ok {
				if td, ok := delta.Delta.AsAny().(anthropic.BetaTextDelta); ok &&
					td.Text != "" {
					text.WriteString(td.Text)
					sse.event("delta", map[string]string{"text": td.Text})
				}
			}
		}
	}
	if err := runner.Err(); err != nil {
		return "", "", false, err
	}

	if last := runner.LastMessage(); last != nil &&
		string(last.StopReason) == "refusal" {
		sse.event("delta", map[string]string{"text": refusalMessage})
		text.Reset()
		text.WriteString(refusalMessage)
	}

	cartID, cartMutated = br.cartState()
	return text.String(), cartID, cartMutated, nil
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
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw)
	s.f.Flush()
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
