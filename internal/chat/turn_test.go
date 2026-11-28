package chat

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcpsrv"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func fakeModelSSE(content string) *http.Response {
	body := `data: {"id":"cmpl-1","object":"chat.completion.chunk",` +
		`"created":1,"model":"m","choices":[{"index":0,` +
		`"delta":{"role":"assistant","content":` +
		`"` + content + `"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"cmpl-1","object":"chat.completion.chunk",` +
		`"created":1,"model":"m","choices":[{"index":0,` +
		`"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// A turn sends the store's profile reasoningEffort unchanged, and omits
// reasoning_effort completely when the profile leaves it empty.
func TestRunTurnSendsProfileReasoningEffort(t *testing.T) {
	cases := []struct {
		name        string
		profile     django.AIProfile
		wantEffort  string
		wantPresent bool
	}{
		{
			name: "medium sent as configured",
			profile: django.AIProfile{
				Provider:        "gemini",
				BaseURL:         "https://generativelanguage.googleapis.com/v1beta/openai/",
				Model:           "gemini-3.5-flash-lite",
				APIKey:          "sk-test",
				ReasoningEffort: "medium",
			},
			wantEffort:  "medium",
			wantPresent: true,
		},
		{
			name: "empty omits reasoning_effort",
			profile: django.AIProfile{
				Provider:        "gemini",
				BaseURL:         "https://generativelanguage.googleapis.com/v1beta/openai/",
				Model:           "gemini-3.5-flash-lite",
				APIKey:          "sk-test",
				ReasoningEffort: "",
			},
			wantPresent: false,
		},
		{
			name: "sent unchanged without provider or model filtering",
			profile: django.AIProfile{
				Provider:        "openai",
				BaseURL:         "https://api.openai.com/v1/",
				Model:           "gpt-4.1-mini",
				APIKey:          "sk-test",
				ReasoningEffort: "medium",
			},
			wantEffort:  "medium",
			wantPresent: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			httpClient := &http.Client{
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					raw, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.NoError(t, json.Unmarshal(raw, &gotBody))
					return fakeModelSSE("Hello!"), nil
				}),
			}
			log := slog.New(slog.DiscardHandler)
			server := mcpsrv.NewChatServer(mcpsrv.Deps{Version: "test", Log: log})
			cfg := config.Config{
				ChatMaxTokens:     512,
				ChatMaxIterations: 2,
				ChatTurnTimeout:   5 * time.Second,
			}
			svc := New(cfg, server, nil, nil, log,
				option.WithHTTPClient(httpClient))

			tn := testTenant()
			profile := tc.profile
			tn.AIProfile = &profile
			req := httptest.NewRequest(http.MethodPost, "/chat", nil)
			rec := httptest.NewRecorder()
			res, err := svc.runTurn(req, tn, &Conversation{ID: "c-1"}, "hi",
				&sseWriter{w: rec, f: rec})
			require.NoError(t, err)
			assert.Equal(t, "Hello!", res.text)
			require.NotNil(t, gotBody)
			if tc.wantPresent {
				assert.Equal(t, tc.wantEffort, gotBody["reasoning_effort"])
			} else {
				assert.NotContains(t, gotBody, "reasoning_effort")
			}
		})
	}
}

// A reasoning_effort value the provider rejects surfaces through the
// existing model-error path.
func TestRunTurnSurfacesProviderReasoningEffortRejection(t *testing.T) {
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"error":{"message":"Unsupported parameter: reasoning_effort"}}`)),
				Request: r,
			}, nil
		}),
	}
	log := slog.New(slog.DiscardHandler)
	server := mcpsrv.NewChatServer(mcpsrv.Deps{Version: "test", Log: log})
	cfg := config.Config{
		ChatMaxTokens:     512,
		ChatMaxIterations: 2,
		ChatTurnTimeout:   5 * time.Second,
	}
	svc := New(cfg, server, nil, nil, log, option.WithHTTPClient(httpClient))

	tn := testTenant()
	tn.AIProfile = &django.AIProfile{
		Provider:        "openai",
		BaseURL:         "https://api.openai.com/v1/",
		Model:           "gpt-4.1-mini",
		APIKey:          "sk-test",
		ReasoningEffort: "medium",
	}
	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	rec := httptest.NewRecorder()
	_, err := svc.runTurn(req, tn, &Conversation{ID: "c-1"}, "hi",
		&sseWriter{w: rec, f: rec})
	var apiErr *openai.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
}
