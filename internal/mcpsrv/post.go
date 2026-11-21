package mcpsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
)

// post is the body of a request to /mcp, read once for the middleware in
// front of the MCP handler (the signature check, the account challenge)
// and handed on to it unchanged.
type post struct {
	raw []byte
	// calls is what the body calls, read on first use (toolCalls).
	calls  []calledTool
	parsed bool
}

// calledTool is one tools/call a body makes: the tool, and its arguments
// as sent.
type calledTool struct {
	name string
	args json.RawMessage
}

type postKey struct{}

// readPost reads r's body, bounded as the MCP handler bounds it, and
// gives it back to r. The first middleware to read it keeps it in the
// returned request's context for the next. It answers the request
// itself, and returns false, for a body it cannot read.
func readPost(
	w http.ResponseWriter, r *http.Request,
) (*http.Request, *post, bool) {
	p, ok := r.Context().Value(postKey{}).(*post)
	if !ok {
		raw, err := io.ReadAll(
			http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			httpmw.WriteJSONError(w, http.StatusRequestEntityTooLarge,
				"request body too large")
			return r, nil, false
		}
		if err != nil {
			httpmw.WriteJSONError(w, http.StatusBadRequest,
				"request body could not be read")
			return r, nil, false
		}
		p = &post{raw: raw}
		r = r.WithContext(context.WithValue(r.Context(), postKey{}, p))
	}
	r.Body = io.NopCloser(bytes.NewReader(p.raw))
	return r, p, true
}

// toolCalls are the tools/call messages of the body, read once.
func (p *post) toolCalls() []calledTool {
	if !p.parsed {
		p.calls, p.parsed = readToolCalls(p.raw), true
	}
	return p.calls
}

// replyID is the id an error answering the whole body names: the id of
// a lone request, or null (JSON-RPC 2.0, section 5) for a batch, a
// notification or a body that is not JSON-RPC.
func (p *post) replyID() any {
	if !withinSDKDepth(p.raw) {
		return nil
	}
	msg, err := jsonrpc.DecodeMessage(p.raw)
	if err != nil {
		return nil
	}
	req, ok := msg.(*jsonrpc.Request)
	if !ok || !req.ID.IsValid() {
		return nil
	}
	return req.ID.Raw()
}

// readToolCalls reads the tools/call messages of a request body exactly
// as the MCP SDK's streamable HTTP handler reads it, so that no tool the
// SDK would run from it goes unseen:
//   - a body nested deeper than sdkMaxDepth is not decoded (decodeAsSDK;
//     jsonrpc.DecodeMessage applies the same bound) and calls nothing
//     here. It is passed on, and the SDK refuses it whole, with a 400,
//     before it reads any message in it, so no tool in it runs;
//   - a JSON array is a batch of messages, anything else one message
//     (the SDK's readBatch);
//   - each message is decoded by the SDK's own jsonrpc.DecodeMessage;
//   - a tools/call's params are decoded, with the decoder and options
//     the SDK uses (segmentio/encoding with case-sensitive field names,
//     so `Name` is not `name`), into the two members of the SDK's
//     CallToolParamsRaw read here (toolCallParams). The rest, `_meta`
//     among it, is skipped: any params the SDK decodes are decoded here,
//     so no call it runs goes unseen.
//
// The SDK refuses a batch on protocol 2025-06-18 and later; such a body
// is still read here, and refusing one the SDK would refuse costs
// nothing. A body that is not JSON-RPC calls nothing: the MCP handler
// refuses it.
func readToolCalls(body []byte) []calledTool {
	var batch []json.RawMessage
	raws := []json.RawMessage{body}
	if decodeAsSDK(body, &batch) == nil {
		raws = batch
	}
	var calls []calledTool
	for _, raw := range raws {
		msg, err := jsonrpc.DecodeMessage(raw)
		if err != nil {
			return nil
		}
		req, ok := msg.(*jsonrpc.Request)
		if !ok || req.Method != "tools/call" || req.Params == nil {
			continue
		}
		var params toolCallParams
		if decodeAsSDK(req.Params, &params) == nil {
			calls = append(calls, calledTool{
				name: params.Name, args: params.Arguments,
			})
		}
	}
	return calls
}

// toolCallParams is what readToolCalls reads of a tools/call's params:
// the members of the SDK's CallToolParamsRaw by the same names, and none
// of its others.
type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// calledTools names the tools a request body calls (readToolCalls).
func calledTools(body []byte) []string {
	calls := readToolCalls(body)
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.name)
	}
	return names
}
