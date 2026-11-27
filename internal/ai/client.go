// Package ai is the gateway's shared OpenAI-compatible model client layer.
// Every AI surface (the shopping chatbot, the upcoming page-builder agent)
// calls models through this package using the store's own AIProfile from
// Django's tenant/resolve — never a platform-wide URL, model or key.
package ai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// ErrProfileUnavailable means the store has no usable AI profile.
var ErrProfileUnavailable = errors.New("ai: store has no valid AI profile")

// Usage is the token count reported by a model call.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// Add folds other into u and returns the sum.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		PromptTokens:     u.PromptTokens + other.PromptTokens,
		CompletionTokens: u.CompletionTokens + other.CompletionTokens,
		TotalTokens:      u.TotalTokens + other.TotalTokens,
	}
}

// Client calls OpenAI-compatible chat-completions endpoints using each
// store's own AIProfile per request, sharing one underlying HTTP pool.
type Client struct {
	completions openai.ChatCompletionService
	// overrideOpts apply after per-profile options; empty in production,
	// used by tests to route requests to a fake model server or custom
	// RoundTripper.
	overrideOpts []option.RequestOption
}

// New builds the shared AI client with zero SDK retries and no ambient
// OPENAI_* environment variable inheritance, so a pod's environment can
// never leak org/project headers into tenant provider calls. allowLocal
// matches Config.AllowLocalWebhooks: false in production and staging so
// the dialer refuses non-public addresses as defense-in-depth behind
// Django's fixed provider catalog.
func New(allowLocal bool, overrideOpts ...option.RequestOption) *Client {
	return &Client{
		completions: openai.NewChatCompletionService(
			option.WithHTTPClient(safeHTTPClient(allowLocal)),
			option.WithMaxRetries(0),
		),
		overrideOpts: append([]option.RequestOption(nil), overrideOpts...),
	}
}

var internalHostSuffixes = []string{
	".local",
	".internal",
	".svc",
	".cluster.local",
	".localhost",
}

// specialUse lists RFC 6890 special-purpose ranges netip's predicates
// leave out (shared address space, documentation, benchmarking, reserved).
var specialUse = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func publicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, p := range specialUse {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

func safeHTTPClient(allowLocal bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowLocal {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			addr, err := netip.ParseAddr(host)
			if err != nil || !publicAddr(addr) {
				return fmt.Errorf("%w: %s is not publicly routable",
					ErrProfileUnavailable, host)
			}
			return nil
		}
	}
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
}

func isPrintableNonSpaceASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < 0x21 || s[i] > 0x7E {
			return false
		}
	}
	return true
}

// ValidateProfile checks that p is complete, its key/fields are clean
// printable non-space ASCII, and its base URL is a publicly routable HTTPS
// URL (defense-in-depth behind Django's fixed platform provider catalog).
func ValidateProfile(p *django.AIProfile) error {
	if p == nil {
		return ErrProfileUnavailable
	}
	if !isPrintableNonSpaceASCII(p.Provider) ||
		!isPrintableNonSpaceASCII(p.Model) ||
		!isPrintableNonSpaceASCII(p.APIKey) ||
		!isPrintableNonSpaceASCII(p.BaseURL) {
		return ErrProfileUnavailable
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil ||
		u.Scheme != "https" ||
		u.Hostname() == "" ||
		u.User != nil ||
		u.RawQuery != "" ||
		u.ForceQuery ||
		u.Fragment != "" {
		return fmt.Errorf("%w: invalid provider base URL", ErrProfileUnavailable)
	}
	host := u.Hostname()
	if addr, err := netip.ParseAddr(host); err == nil {
		if !publicAddr(addr) {
			return fmt.Errorf("%w: non-public provider address",
				ErrProfileUnavailable)
		}
		return nil
	}
	lower := strings.ToLower(host)
	if !strings.Contains(lower, ".") {
		return fmt.Errorf("%w: non-public provider host", ErrProfileUnavailable)
	}
	for _, suffix := range internalHostSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return fmt.Errorf("%w: internal provider host",
				ErrProfileUnavailable)
		}
	}
	return nil
}

// ProfileReady reports whether p is valid and ready for model calls.
func ProfileReady(p *django.AIProfile) bool {
	return ValidateProfile(p) == nil
}

func hasModelFamily(model, prefix string) bool {
	if !strings.HasPrefix(model, prefix) {
		return false
	}
	if len(model) == len(prefix) {
		return true
	}
	next := model[len(prefix)]
	return next == '-' || next == '.' || next == ':'
}

// AcceptsReasoningEffort reports whether provider+model accepts the given
// OpenAI reasoning_effort value without rejecting it. OpenAI rejects
// reasoning_effort on non-reasoning models (gpt-4.1, gpt-4o) and restricts
// allowed effort levels per family (gpt-5: minimal..high; o1/o3/o4:
// low..high). Gemini's OpenAI-compatible endpoint accepts none..high.
func AcceptsReasoningEffort(
	provider, model string, effort shared.ReasoningEffort,
) bool {
	if effort == "" {
		return false
	}
	switch provider {
	case "gemini":
		switch effort {
		case shared.ReasoningEffortNone,
			shared.ReasoningEffortMinimal,
			shared.ReasoningEffortLow,
			shared.ReasoningEffortMedium,
			shared.ReasoningEffortHigh:
			return true
		default:
			return false
		}
	case "openai":
		m := strings.ToLower(model)
		switch {
		case hasModelFamily(m, "gpt-5"):
			switch effort {
			case shared.ReasoningEffortMinimal,
				shared.ReasoningEffortLow,
				shared.ReasoningEffortMedium,
				shared.ReasoningEffortHigh:
				return true
			default:
				return false
			}
		case hasModelFamily(m, "o1"),
			hasModelFamily(m, "o3"),
			hasModelFamily(m, "o4"):
			switch effort {
			case shared.ReasoningEffortLow,
				shared.ReasoningEffortMedium,
				shared.ReasoningEffortHigh:
				return true
			default:
				return false
			}
		default:
			return false
		}
	default:
		return false
	}
}

// SupportsStreamUsage reports whether provider accepts OpenAI's
// stream_options.include_usage field. Mistral uses strict Pydantic schema
// validation (extra_forbidden) and rejects stream_options with HTTP 422.
func SupportsStreamUsage(provider string) bool {
	switch provider {
	case "gemini", "openai", "groq", "openrouter":
		return true
	default:
		return false
	}
}

// UsesLegacyMaxTokens reports whether provider expects max_tokens instead
// of max_completion_tokens. Mistral rejects max_completion_tokens with 422;
// Anthropic's compatibility layer uses max_tokens.
func UsesLegacyMaxTokens(provider string) bool {
	switch provider {
	case "mistral", "anthropic":
		return true
	default:
		return false
	}
}

// RequestOptions returns the per-request openai-go options for p.
func (c *Client) RequestOptions(
	p *django.AIProfile,
) ([]option.RequestOption, error) {
	if err := ValidateProfile(p); err != nil {
		return nil, err
	}
	opts := make([]option.RequestOption, 0, 2+len(c.overrideOpts))
	opts = append(opts,
		option.WithBaseURL(p.BaseURL),
		option.WithAPIKey(p.APIKey),
	)
	opts = append(opts, c.overrideOpts...)
	return opts, nil
}

// NewStreaming opens a streaming chat completion for the store's profile.
// It pins params.Model from p, normalises token-limit, stream_options,
// reasoning_effort and tool fields for the store's provider and model.
func (c *Client) NewStreaming(
	ctx context.Context,
	p *django.AIProfile,
	params openai.ChatCompletionNewParams,
) (*ssestream.Stream[openai.ChatCompletionChunk], error) {
	opts, err := c.RequestOptions(p)
	if err != nil {
		return nil, err
	}
	params.Model = p.Model
	if SupportsStreamUsage(p.Provider) {
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		}
	} else {
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{}
	}
	if UsesLegacyMaxTokens(p.Provider) && params.MaxCompletionTokens.Valid() {
		params.MaxTokens = params.MaxCompletionTokens
		params.MaxCompletionTokens = param.Opt[int64]{}
	}
	if !p.SupportsToolCalling {
		params.Tools = nil
		params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{}
	}
	if !AcceptsReasoningEffort(p.Provider, p.Model, params.ReasoningEffort) {
		params.ReasoningEffort = shared.ReasoningEffort("")
	}
	return c.completions.NewStreaming(ctx, params, opts...), nil
}

// UsageFrom extracts token counts from a completed stream accumulator.
func UsageFrom(acc *openai.ChatCompletionAccumulator) Usage {
	if acc == nil {
		return Usage{}
	}
	return Usage{
		PromptTokens:     acc.Usage.PromptTokens,
		CompletionTokens: acc.Usage.CompletionTokens,
		TotalTokens:      acc.Usage.TotalTokens,
	}
}
