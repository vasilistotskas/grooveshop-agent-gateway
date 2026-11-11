package chat

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcpsrv"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

type chatRequest struct {
	ConversationID string `json:"conversationId"`
	Message        string `json:"message"`
	CartID         string `json:"cartId"`
	// CartToken is a signed-in shopper's cart pass (django.CartToken).
	// It lives only in this turn's context, never in the conversation.
	// A pointer, so a supplied empty token is refused like any other
	// malformed one instead of passing for an omitted field.
	CartToken *string `json:"cartToken"`
	// TurnID names the turn: the widget mints one per message and sends
	// it again when the shopper retries that message, so the retry's cart
	// changes are recognised as the same (mcpsrv.Turn). A turn without
	// one is named here, and only its own repeats are recognised.
	TurnID string `json:"turnId"`
	// Locale is the language of the storefront page the shopper is on
	// (e.g. "el", "en"). The turn is served in it when the store serves
	// that language, else in the store's default.
	Locale string `json:"locale"`
	// Attribution is the shopper's first touch as the storefront captured
	// it for the tab. The cart the turn's add_to_cart first fills records
	// it as its source; without it Django records the cart as direct.
	// It lives only in this turn's context, never in the conversation.
	Attribution *chatAttribution `json:"attribution"`
}

// chatAttribution is Django's OrderAttributionInput without
// agentProtocol, the one field Django believes from the gateway alone
// (it books a UCP/ACP checkout as the agent's): a shopper's request has
// no way to set it. Django checks the shape and never the content, so a
// cart add never fails over a strange referrer; the typed decode is
// that shape check here.
type chatAttribution struct {
	UtmSource   string   `json:"utmSource"`
	UtmMedium   string   `json:"utmMedium"`
	UtmCampaign string   `json:"utmCampaign"`
	ClickIDs    []string `json:"clickIds"`
	Referrer    string   `json:"referrer"`
	LandingPath string   `json:"landingPath"`
}

// toDjango is the attribution the turn's cart adds send; nil without
// one.
func (a *chatAttribution) toDjango() *django.Attribution {
	if a == nil {
		return nil
	}
	return &django.Attribution{
		UtmSource:   a.UtmSource,
		UtmMedium:   a.UtmMedium,
		UtmCampaign: a.UtmCampaign,
		ClickIDs:    a.ClickIDs,
		Referrer:    a.Referrer,
		LandingPath: a.LandingPath,
	}
}

// parseRequest decodes and checks a turn's request, and puts on the
// request's context what the turn's tools read from it: the language,
// the turn, the cart token and the attribution. lang is the turn's
// language. A request it refuses it has answered, and ok is false.
func (s *Service) parseRequest(
	w http.ResponseWriter, r *http.Request, t *tenant.Tenant,
) (_ *http.Request, req chatRequest, lang string,
	cartToken *django.CartToken, ok bool) {
	// The widget renders payload.error verbatim, so every refusal below
	// speaks the turn's language: the store's default until the request
	// names a language the store serves.
	lang = t.DefaultLocale
	fail := func(key string) {
		httpmw.WriteJSONError(w, http.StatusBadRequest, messageFor(lang, key))
	}
	if err := json.NewDecoder(
		http.MaxBytesReader(w, r.Body, 64<<10),
	).Decode(&req); err != nil {
		fail(msgBadRequest)
		return nil, req, lang, nil, false
	}
	if req.Locale != "" && t.Serves(req.Locale) {
		lang = req.Locale
	}
	// Every Django call the turn's tools make asks for this language, so
	// product names come back in the page's language (django.Language).
	r = r.WithContext(django.WithLanguage(r.Context(), lang))
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		fail(msgMessageEmpty)
		return nil, req, lang, nil, false
	}
	if utf8.RuneCountInString(req.Message) > s.cfg.ChatMaxMessageLen {
		fail(msgMessageTooLong)
		return nil, req, lang, nil, false
	}
	// The cart id lands in the system prompt, so it must be exactly what
	// the widget sends — a cart UUID — and never free text carrying
	// instructions at system-role authority.
	if req.CartID != "" && uuid.Validate(req.CartID) != nil {
		fail(msgBadRequest)
		return nil, req, lang, nil, false
	}
	if req.TurnID == "" {
		req.TurnID = uuid.NewString()
	} else if uuid.Validate(req.TurnID) != nil {
		fail(msgBadRequest)
		return nil, req, lang, nil, false
	}
	// Tool handlers reach the turn through the bridge's base context.
	r = r.WithContext(mcpsrv.WithTurn(r.Context(), mcpsrv.NewTurn(req.TurnID)))
	if req.CartToken != nil {
		var err error
		if cartToken, err = django.ParseCartToken(*req.CartToken); err != nil {
			fail(msgBadRequest)
			return nil, req, lang, nil, false
		}
		// Every cart call of the turn acts on the token's cart; tool
		// handlers reach it through the bridge's base context.
		r = r.WithContext(django.WithCartToken(r.Context(), cartToken))
	}
	if attribution := req.Attribution.toDjango(); attribution != nil {
		r = r.WithContext(
			mcpsrv.WithShopperAttribution(r.Context(), attribution))
	}
	return r, req, lang, cartToken, true
}
