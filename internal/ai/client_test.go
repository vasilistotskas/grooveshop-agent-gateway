package ai

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func sseResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestValidateProfile(t *testing.T) {
	valid := &django.AIProfile{
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1/",
		Model:    "gpt-4.1-mini",
		APIKey:   "sk-test",
	}
	require.NoError(t, ValidateProfile(valid))
	assert.True(t, ProfileReady(valid))

	for name, p := range map[string]*django.AIProfile{
		"nil":                nil,
		"empty provider":     {BaseURL: "https://api.openai.com/v1/", Model: "m", APIKey: "k"},
		"empty model":        {Provider: "openai", BaseURL: "https://api.openai.com/v1/", APIKey: "k"},
		"empty key":          {Provider: "openai", BaseURL: "https://api.openai.com/v1/", Model: "m"},
		"space in key":       {Provider: "openai", BaseURL: "https://api.openai.com/v1/", Model: "m", APIKey: "sk bad"},
		"crlf in key":        {Provider: "openai", BaseURL: "https://api.openai.com/v1/", Model: "m", APIKey: "sk\r\nX: 1"},
		"trailing space url": {Provider: "openai", BaseURL: "https://api.openai.com/v1/ ", Model: "m", APIKey: "k"},
		"http base url":      {Provider: "openai", BaseURL: "http://api.openai.com/v1/", Model: "m", APIKey: "k"},
		"query in url":       {Provider: "openai", BaseURL: "https://api.openai.com/v1/?x=1", Model: "m", APIKey: "k"},
		"userinfo in url":    {Provider: "openai", BaseURL: "https://u:p@api.openai.com/v1/", Model: "m", APIKey: "k"},
		"fragment in url":    {Provider: "openai", BaseURL: "https://api.openai.com/v1/#f", Model: "m", APIKey: "k"},
		"missing hostname":   {Provider: "openai", BaseURL: "https:///v1/", Model: "m", APIKey: "k"},
		"loopback ip":        {Provider: "openai", BaseURL: "https://127.0.0.1/v1/", Model: "m", APIKey: "k"},
		"ipv6 loopback":      {Provider: "openai", BaseURL: "https://[::1]/v1/", Model: "m", APIKey: "k"},
		"private ip":         {Provider: "openai", BaseURL: "https://10.0.0.1/v1/", Model: "m", APIKey: "k"},
		"metadata ip":        {Provider: "openai", BaseURL: "https://169.254.169.254/v1/", Model: "m", APIKey: "k"},
		"localhost":          {Provider: "openai", BaseURL: "https://localhost/v1/", Model: "m", APIKey: "k"},
		"cluster svc host":   {Provider: "openai", BaseURL: "https://django.grooveshop.svc/v1/", Model: "m", APIKey: "k"},
		"internal suffix":    {Provider: "openai", BaseURL: "https://llm.internal/v1/", Model: "m", APIKey: "k"},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, ValidateProfile(p), ErrProfileUnavailable)
			assert.False(t, ProfileReady(p))
		})
	}
}

func TestAcceptsReasoningEffort(t *testing.T) {
	assert.True(t, AcceptsReasoningEffort("gemini", "gemini-3.5-flash-lite", shared.ReasoningEffortLow))
	assert.True(t, AcceptsReasoningEffort("gemini", "gemini-3.5-flash-lite", shared.ReasoningEffortMinimal))
	assert.False(t, AcceptsReasoningEffort("gemini", "gemini-3.5-flash-lite", shared.ReasoningEffortMax))
	assert.False(t, AcceptsReasoningEffort("gemini", "gemini-3.5-flash-lite", shared.ReasoningEffortXhigh))

	assert.True(t, AcceptsReasoningEffort("openai", "gpt-5-mini", shared.ReasoningEffortMinimal))
	assert.True(t, AcceptsReasoningEffort("openai", "gpt-5-mini", shared.ReasoningEffortLow))
	assert.True(t, AcceptsReasoningEffort("openai", "gpt-5", shared.ReasoningEffortHigh))
	assert.False(t, AcceptsReasoningEffort("openai", "gpt-5-mini", shared.ReasoningEffortNone))
	assert.False(t, AcceptsReasoningEffort("openai", "gpt-5-mini", shared.ReasoningEffortXhigh))
	assert.False(t, AcceptsReasoningEffort("openai", "gpt-5-mini", shared.ReasoningEffortMax))

	assert.True(t, AcceptsReasoningEffort("openai", "o1", shared.ReasoningEffortLow))
	assert.True(t, AcceptsReasoningEffort("openai", "o3-mini", shared.ReasoningEffortMedium))
	assert.True(t, AcceptsReasoningEffort("openai", "o4-mini", shared.ReasoningEffortHigh))
	assert.False(t, AcceptsReasoningEffort("openai", "o3-mini", shared.ReasoningEffortMinimal))
	assert.False(t, AcceptsReasoningEffort("openai", "o3-mini", shared.ReasoningEffortNone))

	assert.False(t, AcceptsReasoningEffort("openai", "gpt-4.1-mini", shared.ReasoningEffortLow))
	assert.False(t, AcceptsReasoningEffort("openai", "gpt-4o", shared.ReasoningEffortLow))
	assert.False(t, AcceptsReasoningEffort("mistral", "mistral-small-latest", shared.ReasoningEffortLow))
	assert.False(t, AcceptsReasoningEffort("anthropic", "claude-sonnet-4-6", shared.ReasoningEffortLow))
	assert.False(t, AcceptsReasoningEffort("groq", "llama-3.3-70b-versatile", shared.ReasoningEffortLow))
}

func TestProviderRequestShaping(t *testing.T) {
	for _, provider := range []string{
		"openai", "gemini", "anthropic", "groq", "openrouter",
	} {
		assert.True(t, SupportsStreamUsage(provider), provider)
		assert.False(t, UsesMaxTokens(provider), provider)
	}
	assert.False(t, SupportsStreamUsage("mistral"))
	assert.True(t, UsesMaxTokens("mistral"))
}

func TestNewStreamingRoutesPerProfileAndNormalisesRequest(t *testing.T) {
	// Ambient OPENAI_* env vars must never leak into tenant requests.
	t.Setenv("OPENAI_ORG_ID", "org-should-never-leak")
	t.Setenv("OPENAI_PROJECT_ID", "proj-should-never-leak")
	t.Setenv("OPENAI_CUSTOM_HEADERS", "X-Leak: bad")

	type recorded struct {
		url     string
		auth    string
		headers http.Header
		body    map[string]any
	}
	var got []recorded
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			var payload map[string]any
			require.NoError(t, json.Unmarshal(raw, &payload))
			got = append(got, recorded{
				url:     r.URL.String(),
				auth:    r.Header.Get("Authorization"),
				headers: r.Header.Clone(),
				body:    payload,
			})
			sse := "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\"," +
				"\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0," +
				"\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n" +
				"data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\"," +
				"\"created\":1,\"model\":\"m\",\"choices\":[]," +
				"\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5,\"total_tokens\":17}}\n\n" +
				"data: [DONE]\n\n"
			return sseResponse(sse), nil
		}),
	}
	client := New(false, option.WithHTTPClient(httpClient))

	tool := openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name: "search_products",
	})
	baseParams := func() openai.ChatCompletionNewParams {
		return openai.ChatCompletionNewParams{
			Messages:            []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
			Tools:               []openai.ChatCompletionToolUnionParam{tool},
			MaxCompletionTokens: openai.Int(512),
			ReasoningEffort:     shared.ReasoningEffortLow,
		}
	}

	// Store A: OpenAI non-reasoning model (gpt-4.1-mini) — tools + usage,
	// max_completion_tokens, NO reasoning_effort (OpenAI rejects it with 400).
	pA := &django.AIProfile{
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1/",
		Model:    "gpt-4.1-mini",
		APIKey:   "sk-store-a",
	}
	streamA, err := client.NewStreaming(t.Context(), pA, baseParams())
	require.NoError(t, err)
	accA := openai.ChatCompletionAccumulator{}
	for streamA.Next() {
		require.True(t, accA.AddChunk(streamA.Current()))
	}
	require.NoError(t, streamA.Err())
	assert.Equal(t, Usage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17}, UsageFrom(&accA))

	// Store B: OpenAI reasoning model (gpt-5-mini) — keeps supported low effort.
	pB := &django.AIProfile{
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1/",
		Model:    "gpt-5-mini",
		APIKey:   "sk-store-b",
	}
	streamB, err := client.NewStreaming(t.Context(), pB, baseParams())
	require.NoError(t, err)
	for streamB.Next() {
		_ = streamB.Current()
	}
	require.NoError(t, streamB.Err())

	// Store B with unsupported effort (max) — stripped so OpenAI does not 400.
	unsupportedEffort := baseParams()
	unsupportedEffort.ReasoningEffort = shared.ReasoningEffortMax
	streamBUnsupported, err := client.NewStreaming(t.Context(), pB, unsupportedEffort)
	require.NoError(t, err)
	for streamBUnsupported.Next() {
		_ = streamBUnsupported.Current()
	}
	require.NoError(t, streamBUnsupported.Err())

	// Store C: Mistral — max_tokens instead of max_completion_tokens, no
	// stream_options, tools always sent, no reasoning_effort.
	pC := &django.AIProfile{
		Provider: "mistral",
		BaseURL:  "https://api.mistral.ai/v1/",
		Model:    "mistral-small-latest",
		APIKey:   "sk-store-c",
	}
	streamC, err := client.NewStreaming(t.Context(), pC, baseParams())
	require.NoError(t, err)
	for streamC.Next() {
		_ = streamC.Current()
	}
	require.NoError(t, streamC.Err())

	// Store D: Anthropic OpenAI-compatible endpoint — max_completion_tokens
	// and stream_options are both supported, tools always sent.
	pD := &django.AIProfile{
		Provider: "anthropic",
		BaseURL:  "https://api.anthropic.com/v1/",
		Model:    "claude-sonnet-4-6",
		APIKey:   "sk-store-d",
	}
	streamD, err := client.NewStreaming(t.Context(), pD, baseParams())
	require.NoError(t, err)
	for streamD.Next() {
		_ = streamD.Current()
	}
	require.NoError(t, streamD.Err())

	require.Len(t, got, 5)

	// Ambient env headers must never appear on any request.
	for _, r := range got {
		assert.Empty(t, r.headers.Get("OpenAI-Organization"))
		assert.Empty(t, r.headers.Get("OpenAI-Project"))
		assert.Empty(t, r.headers.Get("X-Leak"))
	}

	assert.Equal(t, "https://api.openai.com/v1/chat/completions", got[0].url)
	assert.Equal(t, "Bearer sk-store-a", got[0].auth)
	assert.Equal(t, "gpt-4.1-mini", got[0].body["model"])
	assert.Contains(t, got[0].body, "tools")
	assert.Contains(t, got[0].body, "max_completion_tokens")
	assert.Contains(t, got[0].body, "stream_options")
	assert.NotContains(t, got[0].body, "reasoning_effort")

	assert.Equal(t, "https://api.openai.com/v1/chat/completions", got[1].url)
	assert.Equal(t, "Bearer sk-store-b", got[1].auth)
	assert.Equal(t, "gpt-5-mini", got[1].body["model"])
	assert.Equal(t, "low", got[1].body["reasoning_effort"])

	assert.Equal(t, "https://api.openai.com/v1/chat/completions", got[2].url)
	assert.Equal(t, "gpt-5-mini", got[2].body["model"])
	assert.NotContains(t, got[2].body, "reasoning_effort")

	assert.Equal(t, "https://api.mistral.ai/v1/chat/completions", got[3].url)
	assert.Equal(t, "Bearer sk-store-c", got[3].auth)
	assert.Equal(t, "mistral-small-latest", got[3].body["model"])
	assert.EqualValues(t, 512, got[3].body["max_tokens"])
	assert.NotContains(t, got[3].body, "max_completion_tokens")
	assert.NotContains(t, got[3].body, "stream_options")
	assert.Contains(t, got[3].body, "tools")
	assert.NotContains(t, got[3].body, "reasoning_effort")

	assert.Equal(t, "https://api.anthropic.com/v1/chat/completions", got[4].url)
	assert.Equal(t, "Bearer sk-store-d", got[4].auth)
	assert.Equal(t, "claude-sonnet-4-6", got[4].body["model"])
	assert.EqualValues(t, 512, got[4].body["max_completion_tokens"])
	assert.NotContains(t, got[4].body, "max_tokens")
	assert.Contains(t, got[4].body, "stream_options")
	assert.Contains(t, got[4].body, "tools")
	assert.NotContains(t, got[4].body, "reasoning_effort")
}

func TestProviderRedirectFailsTheCall(t *testing.T) {
	client := safeHTTPClient(true)
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": []string{"https://elsewhere.example/v1/"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	})
	resp, err := client.Get("https://api.example.com/v1/chat/completions")
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorIs(t, err, ErrProviderRedirect)
}
