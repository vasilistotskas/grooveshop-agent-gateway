package mcpsrv

import (
	"bytes"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	sdkjson "github.com/segmentio/encoding/json"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// accountScopes names the account tools and the scope each one needs,
// read off the toolset (account).
var accountScopes = sync.OnceValue(func() map[string]string {
	scopes := map[string]string{}
	for _, t := range definitions() {
		if t.scope != "" {
			scopes[t.def.Name] = t.scope
		}
	}
	return scopes
})

// needsAccount reports whether a store with features serves the named
// tool and it needs a linked shopper account.
func needsAccount(name string, f django.StoreFeatures) bool {
	_, ok := accountScopes()[name]
	return ok && Offered(name, f)
}

// linkScopes is what one link asks for: every account tool the store serves,
// and the profile that identifies the shopper. Tokens are opaque here, so the
// gateway cannot step a narrower grant up later, and a client would be left
// with a tool error it cannot act on. The scopes are all read-only, and the
// shopper can still narrow them on the consent screen.
func linkScopes(f django.StoreFeatures) string {
	scopes := []string{"profile"}
	for tool, scope := range accountScopes() {
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
		r, body, ok := readPost(w, r)
		if !ok {
			return
		}

		// Any account tool the body calls, in a batch too. A tool the
		// store does not serve is unknown, not locked.
		if !slices.ContainsFunc(body.toolCalls(), func(c calledTool) bool {
			return needsAccount(c.name, t.Features)
		}) {
			next.ServeHTTP(w, r)
			return
		}
		identity.WriteChallenge(w, storefront.OAuthResourceMetadata(t.Domain),
			`scope="`+linkScopes(t.Features)+`"`,
			"this tool needs a linked account")
	})
}

// sdkMaxDepth is the nesting the MCP SDK reads a JSON body to:
// defaultMaxDepth in go-sdk v1.8.0 internal/json/json.go, which its
// Unmarshal checks (checkMaxDepth) before it decodes anything.
// TestTheChallengeReaderAppliesTheSDKsNestingBound keeps the two equal.
const sdkMaxDepth = 1000

// errTooDeep is a body nested deeper than sdkMaxDepth.
var errTooDeep = errors.New("mcpsrv: nested deeper than the MCP SDK reads")

// withinSDKDepth reports whether data nests no deeper than sdkMaxDepth,
// scanned byte for byte as the SDK's checkMaxDepth scans it: outside a
// string '{' and '[' open a level and '}' and ']' close one, never below
// zero; a string runs from '"' to the next '"' a backslash does not
// escape. It does not validate JSON: the decoder does.
func withinSDKDepth(data []byte) bool {
	depth, inString, escaped := 0, false, false
	for _, b := range data {
		if inString {
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > sdkMaxDepth {
				return false
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return true
}

// decodeAsSDK decodes data into v as the MCP SDK's internal/json Unmarshal
// does: data nested deeper than sdkMaxDepth is refused before any of it is
// decoded, the rest is decoded by segmentio/encoding with case-sensitive
// field names.
func decodeAsSDK(data []byte, v any) error {
	if !withinSDKDepth(data) {
		return errTooDeep
	}
	dec := sdkjson.NewDecoder(bytes.NewReader(data))
	dec.DontMatchCaseInsensitiveStructFields()
	return dec.Decode(v)
}
