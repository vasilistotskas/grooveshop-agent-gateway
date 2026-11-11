package mcpsrv

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkjson "github.com/segmentio/encoding/json"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// accountScopes names the account tools and the scope each one needs.
var accountScopes = map[string]string{
	"my_orders":         "orders:read",
	"my_loyalty_points": "loyalty:read",
	"my_favourites":     "favourites:read",
}

// IsAccountTool reports whether a tool needs a linked shopper account.
func IsAccountTool(name string) bool {
	_, ok := accountScopes[name]
	return ok
}

// linkScopes is what one link asks for: every account tool the store serves,
// and the profile that identifies the shopper. Tokens are opaque here, so the
// gateway cannot step a narrower grant up later, and a client would be left
// with a tool error it cannot act on. The scopes are all read-only, and the
// shopper can still narrow them on the consent screen.
func linkScopes(f django.StoreFeatures) string {
	scopes := []string{"profile"}
	for tool, scope := range accountScopes {
		if Offered(tool, f) {
			scopes = append(scopes, scope)
		}
	}
	slices.Sort(scopes)
	return strings.Join(scopes, " ")
}

// RequireLinkedAccount answers an anonymous call to an account tool with
// 401 and the RFC 9728 challenge. A client starts OAuth on that status and
// nothing else: a tool error reaches the model as text, which cannot link an
// account. The rest of /mcp stays open.
//
// It runs inside the identity middleware, and reads the body only of an
// anonymous POST, which is the one request that can need the challenge.
func RequireLinkedAccount(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, linked := identity.FromContext(r.Context())
		t, known := tenant.FromContext(r.Context())
		if linked || !known || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(
			http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			httpmw.WriteJSONError(w, http.StatusRequestEntityTooLarge,
				"request body too large")
			return
		}
		if err != nil {
			httpmw.WriteJSONError(w, http.StatusBadRequest,
				"request body could not be read")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		// Any account tool the body calls, in a batch too. A tool the
		// store does not serve is unknown, not locked.
		if !slices.ContainsFunc(calledTools(body), func(name string) bool {
			return IsAccountTool(name) && Offered(name, t.Features)
		}) {
			next.ServeHTTP(w, r)
			return
		}
		identity.WriteChallenge(w, storefront.OAuthResourceMetadata(t.Domain),
			`scope="`+linkScopes(t.Features)+`"`,
			"this tool needs a linked account")
	})
}

// calledTools names the tools a request body calls, read exactly as the
// MCP SDK's streamable HTTP handler reads it, so that no body the SDK
// would run an account tool from passes unchallenged:
//   - a JSON array is a batch of messages, anything else one message
//     (the SDK's readBatch);
//   - each message is decoded by the SDK's own jsonrpc.DecodeMessage;
//   - a tools/call's params are decoded into the SDK's CallToolParamsRaw
//     with the decoder and options the SDK uses: segmentio/encoding with
//     case-sensitive field names, so `Name` is not `name`.
//
// The SDK refuses a batch on protocol 2025-06-18 and later, and a body
// nested more than 1000 deep; such a body is still read here, and
// challenging one the SDK would refuse costs nothing. A body that is not
// JSON-RPC calls nothing: the MCP handler refuses it.
func calledTools(body []byte) []string {
	var batch []json.RawMessage
	raws := []json.RawMessage{body}
	if decodeAsSDK(body, &batch) == nil {
		raws = batch
	}
	var names []string
	for _, raw := range raws {
		msg, err := jsonrpc.DecodeMessage(raw)
		if err != nil {
			return nil
		}
		req, ok := msg.(*jsonrpc.Request)
		if !ok || req.Method != "tools/call" || req.Params == nil {
			continue
		}
		var params mcp.CallToolParamsRaw
		if decodeAsSDK(req.Params, &params) == nil {
			names = append(names, params.Name)
		}
	}
	return names
}

// decodeAsSDK decodes data into v as the MCP SDK's internal/json does.
func decodeAsSDK(data []byte, v any) error {
	dec := sdkjson.NewDecoder(bytes.NewReader(data))
	dec.DontMatchCaseInsensitiveStructFields()
	return dec.Decode(v)
}
