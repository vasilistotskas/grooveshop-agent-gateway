package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// refusalMessage is shown when the model declines a request; the widget
// audience is Greek-first.
const refusalMessage = "Λυπάμαι, δεν μπορώ να βοηθήσω με αυτό το αίτημα. " +
	"Μπορώ όμως να σε βοηθήσω να βρεις προϊόντα, να δεις διαθεσιμότητα " +
	"και να ολοκληρώσεις την παραγγελία σου!"

type Service struct {
	cfg    config.Config
	server *mcp.Server
	store  *Store
	client openai.Client
	log    *slog.Logger
}

// New builds the chat service. The model is reached over the
// OpenAI-compatible chat-completions protocol — CHAT_BASE_URL selects the
// provider (Gemini's compatibility endpoint by default; Groq, Mistral,
// OpenRouter, … are config swaps). Extra options are passed to the client
// (tests inject option.WithBaseURL for a fake API).
func New(
	cfg config.Config, server *mcp.Server, store *Store, log *slog.Logger,
	opts ...option.RequestOption,
) *Service {
	clientOpts := append([]option.RequestOption{
		option.WithBaseURL(cfg.ChatBaseURL),
		option.WithAPIKey(cfg.ChatAPIKey),
	}, opts...)
	return &Service{
		cfg:    cfg,
		server: server,
		store:  store,
		client: openai.NewClient(clientOpts...),
		log:    log,
	}
}

// Enabled reports whether the chat surface is configured; without an API
// key the route is not mounted at all.
func (s *Service) Enabled() bool { return s.cfg.ChatAPIKey != "" }

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
		attrs := []any{
			slog.String("tenant", t.SchemaName),
			slog.String("conversation", conv.ID),
			slog.String("error", err.Error()),
		}
		// The status line alone is undebuggable — surface the model
		// API's error body and the failing request shape.
		message := "Η συνομιλία διακόπηκε προσωρινά — δοκίμασε ξανά."
		var apierr *openai.Error
		if errors.As(err, &apierr) {
			upstream := string(apierr.DumpResponse(true))
			attrs = append(attrs,
				slog.Int("upstream_status", apierr.StatusCode),
				// RawJSON misses non-object error bodies (Gemini wraps
				// errors in an ARRAY) — the response dump is the source
				// of truth.
				slog.String("upstream_response",
					truncateStr(upstream, 2048)),
				slog.String("upstream_request", truncateStr(
					redactAuth(string(apierr.DumpRequest(true))), 8192)),
			)
			// Rate limits are the one upstream failure the shopper can
			// act on (wait) — don't present them as a generic outage.
			// Lift which quota died out of the body dump so triage
			// reads one field instead of a 2KB blob.
			if apierr.StatusCode == http.StatusTooManyRequests {
				message = "Ο βοηθός δέχεται πολλές ερωτήσεις αυτή τη " +
					"στιγμή — δοκίμασε ξανά σε λίγο."
				for field, re := range quotaFields {
					if m := re.FindStringSubmatch(upstream); m != nil {
						attrs = append(attrs, slog.String(field, m[1]))
					}
				}
			}
		}
		s.log.ErrorContext(r.Context(), "chat turn failed", attrs...)
		sse.event("error", map[string]string{"message": message})
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

// runTurn executes one assistant turn: the model looping over the commerce
// tools via the in-process MCP bridge, streaming text deltas to the client.
// The loop is bounded by ChatMaxIterations; each iteration is one streamed
// chat completion that either finishes with text or requests tool calls.
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

	messages := make(
		[]openai.ChatCompletionMessageParamUnion, 0, len(conv.Turns)+2)
	messages = append(messages,
		openai.SystemMessage(systemPrompt(t, conv.CartID)))
	for _, turn := range conv.Turns {
		if turn.Role == "assistant" {
			messages = append(messages, openai.AssistantMessage(turn.Text))
		} else {
			messages = append(messages, openai.UserMessage(turn.Text))
		}
	}
	messages = append(messages, openai.UserMessage(userMessage))

	var text strings.Builder
	for range s.cfg.ChatMaxIterations {
		params := openai.ChatCompletionNewParams{
			Model:               s.cfg.ChatModel,
			Messages:            messages,
			Tools:               tools,
			MaxCompletionTokens: openai.Int(int64(s.cfg.ChatMaxTokens)),
		}
		if s.cfg.ChatEffort != "" {
			params.ReasoningEffort = shared.ReasoningEffort(s.cfg.ChatEffort)
		}

		stream := s.client.Chat.Completions.NewStreaming(ctx, params)
		acc := openai.ChatCompletionAccumulator{}
		// Gemini 3 models attach a per-tool-call
		// ``extra_content.google.thought_signature`` that MUST be echoed
		// back on the assistant message — omitting it is a hard 400.
		// It's a non-standard field, so it only survives via the raw
		// extra-fields channel; keyed by tool-call index.
		signatures := map[int]string{}
		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)
			if len(chunk.Choices) > 0 {
				if delta := chunk.Choices[0].Delta.Content; delta != "" {
					text.WriteString(delta)
					sse.event("delta", map[string]string{"text": delta})
				}
				for _, tc := range chunk.Choices[0].Delta.ToolCalls {
					if raw := tc.JSON.ExtraFields["extra_content"].Raw(); raw != "" {
						signatures[int(tc.Index)] = raw
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			return "", "", false, err
		}
		if len(acc.Choices) == 0 {
			break
		}
		msg := acc.Choices[0].Message

		if msg.Refusal != "" {
			sse.event("delta", map[string]string{"text": refusalMessage})
			text.Reset()
			text.WriteString(refusalMessage)
			break
		}
		if len(msg.ToolCalls) == 0 {
			break
		}

		assistant := msg.ToParam()
		if a := assistant.OfAssistant; a != nil {
			for i := range a.ToolCalls {
				raw, ok := signatures[i]
				if !ok {
					continue
				}
				if f := a.ToolCalls[i].OfFunction; f != nil {
					f.SetExtraFields(map[string]any{
						"extra_content": json.RawMessage(raw),
					})
				}
			}
		}
		messages = append(messages, assistant)
		for _, tc := range msg.ToolCalls {
			input := map[string]any{}
			if args := tc.Function.Arguments; args != "" {
				if err := json.Unmarshal([]byte(args), &input); err != nil {
					messages = append(messages, openai.ToolMessage(
						"ERROR: invalid tool arguments", tc.ID))
					continue
				}
			}
			result, err := br.call(ctx, tc.Function.Name, input)
			if err != nil {
				return "", "", false, fmt.Errorf(
					"chat: tool %s: %w", tc.Function.Name, err)
			}
			messages = append(messages, openai.ToolMessage(result, tc.ID))
		}
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
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw)
	s.f.Flush()
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// quotaFields lift the salient parts of a Gemini RESOURCE_EXHAUSTED body
// into structured log fields: which quota died (per-day vs per-minute),
// for which model, at what limit, and the upstream retry hint. Providers
// that phrase 429s differently simply match nothing.
var quotaFields = map[string]*regexp.Regexp{
	"quota_metric": regexp.MustCompile(`"quotaId":\s*"([^"]+)"`),
	"quota_limit":  regexp.MustCompile(`limit:\s*(\d+)`),
	"quota_model":  regexp.MustCompile(`model:\s*([a-z0-9.-]+)`),
	"retry_hint":   regexp.MustCompile(`retry in ([0-9.]+s)`),
}

// redactAuth strips credential header values from a dumped HTTP request
// before it reaches the logs.
var authHeaderRe = regexp.MustCompile(`(?mi)^(Authorization:).*$`)

func redactAuth(dump string) string {
	return authHeaderRe.ReplaceAllString(dump, "$1 [redacted]")
}
