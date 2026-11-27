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
		Provider:                  "openai",
		BaseURL:                   "https://api.openai.com/v1/",
		Model:                     "gpt-4.1-mini",
		APIKey:                    "sk-test",
		SupportsToolCalling:       true,
		SupportsStructuredOutputs: true,
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

func TestSupportsReasoningEffort(t *testing.T) {
	assert.True(t, SupportsReasoningEffort("gemini", "gemini-3.5-flash-lite"))
	assert.True(t, SupportsReasoningEffort("openai", "gpt-5-mini"))
	assert.True(t, SupportsReasoningEffort("openai", "gpt-5"))
	assert.True(t, SupportsReasoningEffort("openai", "o1"))
	assert.True(t, SupportsReasoningEffort("openai", "o3-mini"))
	assert.True(t, SupportsReasoningEffort("openai", "o4-mini"))

	assert.False(t, SupportsReasoningEffort("openai", "gpt-4.1-mini"))
	assert.False(t, SupportsReasoningEffort("openai", "gpt-4o"))
	assert.False(t, SupportsReasoningEffort("mistral", "mistral-small-latest"))
	assert.False(t, SupportsReasoningEffort("anthropic", "claude-sonnet-4-6"))
	assert.False(t, SupportsReasoningEffort("groq", "llama-3.3-70b-versatile"))
}

func TestNewStreamingRoutesPerProfileAndGatesCapabilities(t *testing.T) {
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
		Provider:            "openai",
		BaseURL:             "https://api.openai.com/v1/",
		Model:               "gpt-4.1-mini",
		APIKey:              "sk-store-a",
		SupportsToolCalling: true,
	}
	streamA, err := client.NewStreaming(t.Context(), pA, baseParams())
	require.NoError(t, err)
	accA := openai.ChatCompletionAccumulator{}
	for streamA.Next() {
		require.True(t, accA.AddChunk(streamA.Current()))
	}
	require.NoError(t, streamA.Err())
	assert.Equal(t, Usage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17}, UsageFrom(&accA))

	// Store B: OpenAI reasoning model (gpt-5-mini) — keeps reasoning_effort.
	pB := &django.AIProfile{
		Provider:            "openai",
		BaseURL:             "https://api.openai.com/v1/",
		Model:               "gpt-5-mini",
		APIKey:              "sk-store-b",
		SupportsToolCalling: true,
	}
	streamB, err := client.NewStreaming(t.Context(), pB, baseParams())
	require.NoError(t, err)
	for streamB.Next() {
		_ = streamB.Current()
	}
	require.NoError(t, streamB.Err())

	// Store C: Mistral without tool calling — max_tokens instead of
	// max_completion_tokens, no stream_options, no tools, no reasoning_effort.
	pC := &django.AIProfile{
		Provider:            "mistral",
		BaseURL:             "https://api.mistral.ai/v1/",
		Model:               "mistral-small-latest",
		APIKey:              "sk-store-c",
		SupportsToolCalling: false,
	}
	streamC, err := client.NewStreaming(t.Context(), pC, baseParams())
	require.NoError(t, err)
	for streamC.Next() {
		_ = streamC.Current()
	}
	require.NoError(t, streamC.Err())

	require.Len(t, got, 3)

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

	assert.Equal(t, "https://api.mistral.ai/v1/chat/completions", got[2].url)
	assert.Equal(t, "Bearer sk-store-c", got[2].auth)
	assert.Equal(t, "mistral-small-latest", got[2].body["model"])
	assert.EqualValues(t, 512, got[2].body["max_tokens"])
	assert.NotContains(t, got[2].body, "max_completion_tokens")
	assert.NotContains(t, got[2].body, "stream_options")
	assert.NotContains(t, got[2].body, "tools")
	assert.NotContains(t, got[2].body, "reasoning_effort")
}
