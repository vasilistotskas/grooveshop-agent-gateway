package chat

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

type Service struct {
	cfg    config.Config
	server *mcp.Server
	store  *Store
	quota  *Quota
	// model holds everything but the credential, so every turn shares
	// its connections: the API key is per-tenant (from tenant/resolve)
	// and attached to each request.
	model openai.Client
	log   *slog.Logger
}

// New builds the chat service. The model is reached over the
// OpenAI-compatible chat-completions protocol — CHAT_BASE_URL selects the
// provider (Gemini's compatibility endpoint by default; Groq, Mistral,
// OpenRouter, … are config swaps). Extra options are passed to the client
// (tests inject option.WithBaseURL for a fake API).
func New(
	cfg config.Config, server *mcp.Server, store *Store, quota *Quota,
	log *slog.Logger,
	opts ...option.RequestOption,
) *Service {
	return &Service{
		cfg:    cfg,
		server: server,
		store:  store,
		quota:  quota,
		// No retries: a throttled turn reaches the shopper at once with
		// the provider's wait, and the widget's retry button resends it.
		model: openai.NewClient(append([]option.RequestOption{
			option.WithBaseURL(cfg.ChatBaseURL),
			option.WithMaxRetries(0),
		}, opts...)...),
		log: log,
	}
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

	r, req, lang, cartToken, ok := s.parseRequest(w, r, t)
	if !ok {
		return
	}
	fail := func(status int, key string) {
		httpmw.WriteJSONError(w, status, messageFor(lang, key))
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
	// Counted only once the request and its conversation are valid, so
	// rejected calls spend nothing; refused before any model call.
	retryAfter, err := s.quota.Allow(r.Context(), t.SchemaName,
		httpmw.ClientBucket(r), time.Now())
	if err != nil {
		s.log.ErrorContext(r.Context(), "chat quota check failed",
			slog.String("error", err.Error()))
		fail(http.StatusServiceUnavailable, msgUnavailable)
		return
	}
	if retryAfter > 0 {
		s.log.InfoContext(r.Context(), "chat turn refused by quota")
		w.Header().Set("Retry-After", strconv.Itoa(seconds(retryAfter)))
		fail(http.StatusTooManyRequests, msgRateLimited)
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
	if err != nil &&
		errors.Is(context.Cause(r.Context()), httpmw.ErrShuttingDown) {
		// The pod is stopping: the widget offers the retry, which
		// another pod serves.
		s.log.InfoContext(r.Context(), "chat turn cut by shutdown",
			slog.String("conversation", conv.ID))
		sse.event("error", errorEvent{
			Message: messageFor(lang, msgTurnFailed),
		})
		return
	}
	if err != nil && r.Context().Err() != nil {
		// The shopper pressed stop or left: nobody is reading the stream,
		// and a disconnect is not a server fault.
		s.log.InfoContext(r.Context(), "chat turn abandoned by client",
			slog.String("conversation", conv.ID))
		return
	}
	if err != nil {
		s.failTurn(r.Context(), lang, conv, sse, err)
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

	// A signed-in shopper's cart is already their session's: an id for
	// the widget to adopt belongs to guest carts only.
	doneCartID := conv.CartID
	if cartToken != nil {
		doneCartID = ""
	}
	sse.event("done", doneEvent{
		ConversationID:   conv.ID,
		CartID:           doneCartID,
		CartMutated:      cartMutated,
		CartTokenRefused: cartToken != nil && cartToken.Refused(),
	})
}
