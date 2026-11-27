package chat

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/openai/openai-go/v3/option"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ai"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

type Service struct {
	cfg    config.Config
	server *mcp.Server
	store  *Store
	quota  *Quota
	// ai shares one HTTP connection pool across tenants while configuring
	// each call with the store's own provider base URL, model and API key
	// from tenant/resolve.
	ai  *ai.Client
	log *slog.Logger
}

// New builds the chat service. Every model call uses the store's own
// AIProfile from tenant/resolve through internal/ai. Extra options are
// passed to the AI client (tests inject option.WithBaseURL or a custom
// HTTP client).
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
		ai:     ai.New(cfg.AllowLocalWebhooks, opts...),
		log:    log,
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
	// Chat is a per-tenant capability: no valid AI profile on the tenant
	// config means the store has not enabled AI features (no fallback).
	if err := ai.ValidateProfile(t.AIProfile); err != nil {
		if t.AIProfile != nil {
			s.log.WarnContext(r.Context(),
				"invalid tenant AI profile; chat disabled",
				slog.String("error", err.Error()))
		}
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

	// A token turn drops a guest cart the stored conversation names; the
	// conversation is stored without it however the turn ends.
	dropped := cartToken != nil && conv.CartID != ""
	switch {
	// A signed-in shopper's cart is reached through their token alone,
	// and a conversation keeps none for them: its id, kept, would be
	// named to the turn that follows a sign-out, and the store serves
	// that cart to its owner only. A guest cart an earlier turn made is
	// dropped too: a sign-in merges it away.
	case cartToken != nil:
		conv.CartID = ""
	// The widget's session cart wins: the bot must operate on the cart
	// the shopper sees in the UI.
	case req.CartID != "":
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

	res, err := s.runTurn(r, t, conv, req.Message, sse)
	// The cart a visitor's cart tool named is the conversation's from
	// here on, whether or not the turn finished: the store has it.
	filled := cartToken == nil && res.cartID != ""
	if filled {
		conv.CartID = res.cartID
	}
	cart := turnCart{
		// A signed-in shopper's cart is already their session's: an id
		// for the widget to adopt belongs to guest carts only, and their
		// conversation holds none.
		CartID:           conv.CartID,
		CartMutated:      res.cartMutated,
		CartTokenRefused: cartToken != nil && cartToken.Refused(),
	}
	if err != nil {
		s.endFailed(r.Context(), t, lang, conv, sse, cart, filled, dropped,
			err)
		return
	}

	conv.Turns = append(conv.Turns,
		Turn{Role: "user", Text: req.Message},
		Turn{Role: "assistant", Text: res.text},
	)
	s.save(r.Context(), t, conv)
	sse.event("done", doneEvent{ConversationID: conv.ID, turnCart: cart})
}

// endFailed ends a turn that failed. Whoever still reads the stream gets
// the error event, which says what the turn's tools did to the cart as
// done would. A turn whose tools filled a visitor's cart stores its
// conversation, so the cart is the next turn's too and not one nobody
// claims, and names the conversation in the event once it is stored. A
// token turn that dropped a guest cart (dropped) stores the conversation
// without it. The failed turn's messages are never stored: the retry
// sends them again.
//
// The save comes before the event, so the event never names a
// conversation that is not stored. It ends at the Redis client's own
// timeouts, well inside the shutdown's final window.
func (s *Service) endFailed(
	ctx context.Context, t *tenant.Tenant, lang string, conv *Conversation,
	sse *sseWriter, cart turnCart, filled, dropped bool, err error,
) {
	failure := errorEvent{
		Message:  messageFor(lang, msgTurnFailed),
		turnCart: cart,
	}
	if (filled || dropped) && s.save(ctx, t, conv) && filled {
		failure.ConversationID = conv.ID
	}
	switch {
	case errors.Is(context.Cause(ctx), httpmw.ErrShuttingDown):
		// The pod is stopping: the widget offers the retry, which
		// another pod serves.
		s.log.InfoContext(ctx, "chat turn cut by shutdown",
			slog.String("conversation", conv.ID))
		sse.event("error", failure)
	case ctx.Err() != nil:
		// The shopper pressed stop or left: nobody is reading the stream,
		// and a disconnect is not a server fault.
		s.log.InfoContext(ctx, "chat turn abandoned by client",
			slog.String("conversation", conv.ID))
	default:
		s.failTurn(ctx, lang, conv, sse, failure, err)
	}
}

// save stores the conversation on a context its caller cannot end. The
// turn's cart changes already happened upstream: a client that
// disconnects, or a pod that is stopping, must not leave the
// conversation behind them. It reports whether the conversation is
// stored.
func (s *Service) save(
	ctx context.Context, t *tenant.Tenant, conv *Conversation,
) bool {
	if err := s.store.Save(context.WithoutCancel(ctx),
		t.SchemaName, conv); err != nil {
		s.log.ErrorContext(ctx, "chat save failed",
			slog.String("error", err.Error()))
		return false
	}
	return true
}
