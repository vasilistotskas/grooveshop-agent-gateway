package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// maxToolCallsPerIteration bounds how many tool calls one model response
// may execute; each one is a Django round trip on the tenant's behalf.
const maxToolCallsPerIteration = 8

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
	lang := django.Language(ctx, t.TenantConfig)

	br, err := newBridge(ctx, s.server)
	if err != nil {
		return "", "", false, fmt.Errorf("chat: bridge: %w", err)
	}
	defer br.close()

	tools, err := br.tools(ctx, t.Features)
	if err != nil {
		return "", "", false, fmt.Errorf("chat: list tools: %w", err)
	}

	messages := make(
		[]openai.ChatCompletionMessageParamUnion, 0, len(conv.Turns)+2)
	messages = append(messages,
		openai.SystemMessage(systemPrompt(
			t, lang, conv.CartID, django.HasCartToken(ctx))))
	for _, turn := range conv.Turns {
		if turn.Role == "assistant" {
			messages = append(messages, openai.AssistantMessage(turn.Text))
		} else {
			messages = append(messages, openai.UserMessage(turn.Text))
		}
	}
	messages = append(messages, openai.UserMessage(userMessage))

	// The credential is the tenant's own: a request's options apply after
	// the client's, so the shared ones can never override it.
	apiKey := option.WithAPIKey(t.ChatAPIKey)
	// The turn's deadline bounds its model calls and when a tool call may
	// start. A tool call already running keeps ctx and finishes under its
	// own Django timeout: cut midway, a cart change Django accepted would
	// go unrecorded. A turn can still fail after a cart change; saying so
	// to the shopper is #94, and a retry with the same turnId does not
	// add a product twice (mcpsrv.Turn).
	turnCtx, cancel := context.WithTimeout(ctx, s.cfg.ChatTurnTimeout)
	defer cancel()

	var reply strings.Builder
	for iteration := range s.cfg.ChatMaxIterations {
		last := iteration == s.cfg.ChatMaxIterations-1
		params := openai.ChatCompletionNewParams{
			Model:               s.cfg.ChatModel,
			Messages:            messages,
			Tools:               tools,
			MaxCompletionTokens: openai.Int(int64(s.cfg.ChatMaxTokens)),
		}
		// The last iteration must answer in prose: tool calls requested
		// there would run (a cart change included) with their results
		// never shown to the model or the shopper. A provider that
		// returns some anyway is not obeyed (below).
		if last {
			params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: openai.String(string(
					openai.ChatCompletionToolChoiceOptionAutoNone)),
			}
		}
		if s.cfg.ChatEffort != "" {
			params.ReasoningEffort = shared.ReasoningEffort(s.cfg.ChatEffort)
		}

		acc, signatures, err := s.streamCompletion(
			turnCtx, params, apiKey, &reply, sse)
		if err != nil {
			return "", "", false, err
		}
		if len(acc.Choices) == 0 {
			break
		}
		msg := acc.Choices[0].Message

		if msg.Refusal != "" {
			refusal := messageFor(lang, msgRefusal)
			sse.event("delta", map[string]string{"text": refusal})
			reply.Reset()
			reply.WriteString(refusal)
			break
		}
		if len(msg.ToolCalls) == 0 || last {
			break
		}

		assistant := msg.ToParam()
		echoThoughtSignatures(assistant.OfAssistant, signatures)
		messages = append(messages, assistant)
		messages, err = runToolCalls(
			ctx, turnCtx, br, msg.ToolCalls, messages, sse)
		if err != nil {
			return "", "", false, err
		}
	}

	// The model can exhaust the iteration budget still asking for tools, or
	// return an empty final message — either way no prose reached the
	// shopper. Surface a friendly nudge instead of an empty bubble.
	if reply.Len() == 0 {
		fallback := messageFor(lang, msgTurnIncomplete)
		sse.event("delta", map[string]string{"text": fallback})
		reply.WriteString(fallback)
	}

	cartID, cartMutated = br.cartState()
	return reply.String(), cartID, cartMutated, nil
}

// streamCompletion streams one chat completion: each text delta is
// added to reply and sent to the shopper. It returns the accumulated
// completion and the Gemini thought signatures of its tool calls.
func (s *Service) streamCompletion(
	ctx context.Context,
	params openai.ChatCompletionNewParams,
	apiKey option.RequestOption,
	reply *strings.Builder,
	sse *sseWriter,
) (*openai.ChatCompletionAccumulator, map[int]string, error) {
	stream := s.model.Chat.Completions.NewStreaming(
		ctx, params, apiKey)
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
			_ = stream.Close()
			return nil, nil, errors.New(
				"chat: model stream sent a chunk that cannot be " +
					"accumulated")
		}
		if len(chunk.Choices) > 0 {
			if delta := chunk.Choices[0].Delta.Content; delta != "" {
				reply.WriteString(delta)
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
		return nil, nil, err
	}
	return &acc, signatures, nil
}

// echoThoughtSignatures puts back on the assistant message the thought
// signature each of its tool calls was streamed with (streamCompletion).
func echoThoughtSignatures(
	a *openai.ChatCompletionAssistantMessageParam, signatures map[int]string,
) {
	if a == nil {
		return
	}
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

// runToolCalls runs one response's tool calls in order and appends the
// tool message answering each to messages. A call runs on ctx; turnCtx,
// the turn's deadline, only decides whether the next one may start.
func runToolCalls(
	ctx, turnCtx context.Context,
	br *bridge,
	calls []openai.ChatCompletionMessageToolCallUnion,
	messages []openai.ChatCompletionMessageParamUnion,
	sse *sseWriter,
) ([]openai.ChatCompletionMessageParamUnion, error) {
	for i, tc := range calls {
		// The turn fails here, and its messages are never stored, so
		// the tool calls left unanswered reach no later request.
		if err := turnCtx.Err(); err != nil {
			return nil, fmt.Errorf(
				"chat: turn ended before tool %s: %w",
				tc.Function.Name, err)
		}
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
		result, products, failed, err := br.call(
			ctx, tc.Function.Name, input)
		if err != nil {
			return nil, fmt.Errorf(
				"chat: tool %s: %w", tc.Function.Name, err)
		}
		status := "done"
		if failed {
			status = "error"
		}
		sse.event("tool", map[string]string{
			"name": tc.Function.Name, "status": status,
		})
		if products != nil {
			sse.event("products", products)
		}
		messages = append(messages, openai.ToolMessage(result, tc.ID))
	}
	return messages, nil
}
