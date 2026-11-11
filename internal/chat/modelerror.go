package chat

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/text"
)

// failTurn logs a turn that failed while the request was still live and
// ends its stream with the error event.
func (s *Service) failTurn(
	ctx context.Context, lang string, conv *Conversation, sse *sseWriter,
	err error,
) {
	attrs := []any{
		slog.String("conversation", conv.ID),
		// A provider error's text carries its body: capped like
		// upstream_response.
		slog.String("error", text.Ellipsize(err.Error(), 2048)),
	}
	// The status line alone is undebuggable — surface the model
	// API's error body and the failing request shape.
	failure := errorEvent{
		Message: messageFor(lang, msgTurnFailed),
	}
	if apierr, ok := errors.AsType[*openai.Error](err); ok {
		attrs = append(attrs,
			// Method, URL and headers only: the body is the whole
			// conversation, with whatever personal data the shopper
			// typed, and does not belong in an ERROR log.
			slog.String("upstream_request",
				redactAuth(string(apierr.DumpRequest(false)))),
		)
		s.log.DebugContext(ctx, "chat turn failed request body",
			slog.String("conversation", conv.ID),
			slog.String("upstream_request", text.Ellipsize(
				redactAuth(string(apierr.DumpRequest(true))), 8192)))
	}
	if perr, ok := providerErrorOf(err); ok {
		attrs = append(attrs, slog.String("upstream_response",
			text.Ellipsize(perr.body, 2048)))
		if perr.status != 0 {
			attrs = append(attrs,
				slog.Int("upstream_status", perr.status))
		}
		// Rate limits are the one upstream failure the shopper can
		// act on (wait) — don't present them as a generic outage.
		// Lift which quota died out of the body so triage reads one
		// field instead of a 2KB blob.
		if perr.status == http.StatusTooManyRequests {
			failure = errorEvent{
				Message:    messageFor(lang, msgRateLimited),
				Code:       codeRateLimited,
				RetryAfter: perr.retryAfter(),
			}
			for field, re := range quotaFields {
				if m := re.FindStringSubmatch(perr.body); m != nil {
					attrs = append(attrs, slog.String(field, m[1]))
				}
			}
		}
	}
	s.log.ErrorContext(ctx, "chat turn failed", attrs...)
	sse.event("error", failure)
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

// providerError is the model provider's error a turn ended on, whether it
// answered the request with one or sent one inside the stream.
type providerError struct {
	// status is the HTTP status. Inside the stream it is the error's
	// code when that is a number, quoted or not, as OpenRouter's
	// mid-stream errors carry it; 0 for a named code ("not_found").
	status int
	// body is the response dump, or the stream event's data.
	body string
	// header is the response's; nil inside the stream.
	header http.Header
}

func providerErrorOf(err error) (providerError, bool) {
	if apierr, ok := errors.AsType[*openai.Error](err); ok {
		var header http.Header
		if apierr.Response != nil {
			header = apierr.Response.Header
		}
		// RawJSON misses non-object error bodies (Gemini wraps errors in
		// an ARRAY) — the response dump is the source of truth.
		return providerError{
			status: apierr.StatusCode,
			body:   string(apierr.DumpResponse(true)),
			header: header,
		}, true
	}
	if serr, ok := errors.AsType[*ssestream.StreamError](err); ok {
		var event struct {
			Error struct {
				Code json.RawMessage `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(serr.Event.Data, &event)
		status, _ := strconv.Atoi(strings.Trim(string(event.Error.Code), `"`))
		return providerError{status: status, body: string(serr.Event.Data)},
			true
	}
	return providerError{}, false
}

// retryAfter is how long the model provider asked to wait, in whole
// seconds rounded up: its Retry-After header, else the hint in its body
// (Gemini's "retry in 12.3s"); 0 when it said neither.
func (m providerError) retryAfter() int {
	if s, err := strconv.Atoi(m.header.Get("Retry-After")); err == nil &&
		s > 0 {
		return s
	}
	if hint := quotaFields["retry_hint"].FindStringSubmatch(m.body); hint != nil {
		if d, err := time.ParseDuration(hint[1]); err == nil && d > 0 {
			return seconds(d)
		}
	}
	return 0
}

// seconds rounds d up to whole seconds, at least one.
func seconds(d time.Duration) int {
	return max(1, int((d+time.Second-1)/time.Second))
}

// redactAuth strips credential header values from a dumped HTTP request
// before it reaches the logs.
var authHeaderRe = regexp.MustCompile(`(?mi)^(Authorization:).*$`)

func redactAuth(dump string) string {
	return authHeaderRe.ReplaceAllString(dump, "$1 [redacted]")
}
