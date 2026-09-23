package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/text"
)

type Service struct {
	cfg    config.Config
	server *mcp.Server
	store  *Store
	// clientOpts hold everything but the credential — the API key is
	// per-tenant (from tenant/resolve) and attached per turn.
	clientOpts []option.RequestOption
	log        *slog.Logger
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
	}, opts...)
	return &Service{
		cfg:        cfg,
		server:     server,
		store:      store,
		clientOpts: clientOpts,
		log:        log,
	}
}

// maxToolCallsPerIteration bounds how many tool calls one model response
// may execute; each one is a Django round trip on the tenant's behalf.
const maxToolCallsPerIteration = 8

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
		httpmw.WriteJSONError(w, http.StatusNotFound, "unknown store")
		return
	}
	// Chat is a per-tenant capability: no key on the tenant config means
	// the store has not enabled the assistant.
	if t.ChatAPIKey == "" {
		httpmw.WriteJSONError(w, http.StatusNotFound,
			messageFor(t.DefaultLocale, msgChatDisabled))
		return
	}

	// The widget renders payload.error verbatim, so every refusal below
	// speaks the tenant's language.
	fail := func(status int, key string) {
		httpmw.WriteJSONError(w, status, messageFor(t.DefaultLocale, key))
	}
	var req chatRequest
	if err := json.NewDecoder(
		http.MaxBytesReader(w, r.Body, 64<<10),
	).Decode(&req); err != nil {
		fail(http.StatusBadRequest, msgBadRequest)
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		fail(http.StatusBadRequest, msgMessageEmpty)
		return
	}
	if utf8.RuneCountInString(req.Message) > s.cfg.ChatMaxMessageLen {
		fail(http.StatusBadRequest, msgMessageTooLong)
		return
	}
	// The cart id lands in the system prompt, so it must be exactly what
	// the widget sends — a cart UUID — and never free text carrying
	// instructions at system-role authority.
	if req.CartID != "" && uuid.Validate(req.CartID) != nil {
		fail(http.StatusBadRequest, msgBadRequest)
		return
	}

	conv, err := s.store.Load(r.Context(), t.SchemaName, req.ConversationID)
	switch {
	case errors.Is(err, ErrInvalidConversation):
		fail(http.StatusBadRequest, msgBadRequest)
		return
	case errors.Is(err, ErrConversationFull):
		fail(http.StatusConflict, msgConversation)
		return
	case err != nil:
		s.log.ErrorContext(r.Context(), "chat load failed",
			slog.String("error", err.Error()))
		fail(http.StatusServiceUnavailable, msgUnavailable)
		return
	}
	// The widget's session cart wins: the bot must operate on the cart the
	// shopper sees in the UI.
	if req.CartID != "" {
		conv.CartID = req.CartID
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(http.StatusInternalServerError, msgUnavailable)
		return
	}
	sse := &sseWriter{w: w, f: flusher}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	assistantText, cartID, cartMutated, err := s.runTurn(r, t, conv, req.Message, sse)
	if err != nil && r.Context().Err() != nil {
		// The shopper pressed stop or left: nobody is reading the stream,
		// and a disconnect is not a server fault.
		s.log.InfoContext(r.Context(), "chat turn abandoned by client",
			slog.String("tenant", t.SchemaName),
			slog.String("conversation", conv.ID))
		return
	}
	if err != nil {
		attrs := []any{
			slog.String("tenant", t.SchemaName),
			slog.String("conversation", conv.ID),
			slog.String("error", err.Error()),
		}
		// The status line alone is undebuggable — surface the model
		// API's error body and the failing request shape.
		message := messageFor(t.DefaultLocale, msgTurnFailed)
		if apierr, ok := errors.AsType[*openai.Error](err); ok {
			upstream := string(apierr.DumpResponse(true))
			attrs = append(attrs,
				slog.Int("upstream_status", apierr.StatusCode),
				// RawJSON misses non-object error bodies (Gemini wraps
				// errors in an ARRAY) — the response dump is the source
				// of truth.
				slog.String("upstream_response",
					text.Ellipsize(upstream, 2048)),
				// Method, URL and headers only: the body is the whole
				// conversation, with whatever personal data the shopper
				// typed, and does not belong in an ERROR log.
				slog.String("upstream_request",
					redactAuth(string(apierr.DumpRequest(false)))),
			)
			s.log.DebugContext(r.Context(), "chat turn failed request body",
				slog.String("conversation", conv.ID),
				slog.String("upstream_request", text.Ellipsize(
					redactAuth(string(apierr.DumpRequest(true))), 8192)))
			// Rate limits are the one upstream failure the shopper can
			// act on (wait) — don't present them as a generic outage.
			// Lift which quota died out of the body dump so triage
			// reads one field instead of a 2KB blob.
			if apierr.StatusCode == http.StatusTooManyRequests {
				message = messageFor(t.DefaultLocale, msgRateLimited)
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
	// The turn's cart changes already happened upstream; a client that
	// disconnects after the last delta must not leave history behind
	// them.
	if err := s.store.Save(context.WithoutCancel(r.Context()),
		t.SchemaName, conv); err != nil {
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

	// The credential is the tenant's own — attach it last so it can
	// never be overridden by the shared base options.
	client := openai.NewClient(append(
		slices.Clone(s.clientOpts), option.WithAPIKey(t.ChatAPIKey))...)

	var text strings.Builder
	for iteration := range s.cfg.ChatMaxIterations {
		params := openai.ChatCompletionNewParams{
			Model:               s.cfg.ChatModel,
			Messages:            messages,
			Tools:               tools,
			MaxCompletionTokens: openai.Int(int64(s.cfg.ChatMaxTokens)),
		}
		// The last iteration must answer in prose: tool calls requested
		// there would run (a cart change included) with their results
		// never shown to the model or the shopper.
		if iteration == s.cfg.ChatMaxIterations-1 {
			params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: openai.String(string(
					openai.ChatCompletionToolChoiceOptionAutoNone)),
			}
		}
		if s.cfg.ChatEffort != "" {
			params.ReasoningEffort = shared.ReasoningEffort(s.cfg.ChatEffort)
		}

		stream := client.Chat.Completions.NewStreaming(ctx, params)
		acc := openai.ChatCompletionAccumulator{}
		// Gemini 3 models attach a per-tool-call
		// ``extra_content.google.thought_signature`` that MUST be echoed
		// back on the assistant message — omitting it is a hard 400.
		// It's a non-standard field, so it only survives via the raw
		// extra-fields channel; keyed by tool-call index.
		signatures := map[int]string{}
		for stream.Next() {
			chunk := stream.Current()
			// A rejected chunk (a foreign id, an out-of-range index) is
			// dropped by the accumulator, tool calls included — the turn
			// would act on a partial request.
			if !acc.AddChunk(chunk) {
				return "", "", false, errors.New(
					"chat: model stream sent a chunk that cannot be " +
						"accumulated")
			}
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
			refusal := messageFor(t.DefaultLocale, msgRefusal)
			sse.event("delta", map[string]string{"text": refusal})
			text.Reset()
			text.WriteString(refusal)
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
		for i, tc := range msg.ToolCalls {
			// Every tool call id needs its tool message, so the excess is
			// answered with an error rather than dropped.
			if i >= maxToolCallsPerIteration {
				messages = append(messages, openai.ToolMessage(
					"ERROR: too many tool calls at once; make at most "+
						strconv.Itoa(maxToolCallsPerIteration)+
						" per step", tc.ID))
				continue
			}
			input := map[string]any{}
			if args := tc.Function.Arguments; args != "" {
				if err := json.Unmarshal([]byte(args), &input); err != nil {
					messages = append(messages, openai.ToolMessage(
						"ERROR: invalid tool arguments", tc.ID))
					continue
				}
			}
			// Tool activity streams to the widget so waits read as
			// progress ("searching products…") instead of dead air.
			sse.event("tool", map[string]string{
				"name": tc.Function.Name, "status": "running",
			})
			result, err := br.call(ctx, tc.Function.Name, input)
			if err != nil {
				return "", "", false, fmt.Errorf(
					"chat: tool %s: %w", tc.Function.Name, err)
			}
			sse.event("tool", map[string]string{
				"name": tc.Function.Name, "status": "done",
			})
			messages = append(messages, openai.ToolMessage(result, tc.ID))
		}
	}

	// The model can exhaust the iteration budget still asking for tools, or
	// return an empty final message — either way no prose reached the
	// shopper. Surface a friendly nudge instead of an empty bubble.
	if text.Len() == 0 {
		fallback := messageFor(t.DefaultLocale, msgTurnIncomplete)
		sse.event("delta", map[string]string{"text": fallback})
		text.WriteString(fallback)
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
