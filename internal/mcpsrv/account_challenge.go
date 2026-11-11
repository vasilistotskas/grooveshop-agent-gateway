package mcpsrv

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
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

		var call struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		// Anything unreadable is the MCP handler's to refuse.
		if json.Unmarshal(body, &call) != nil ||
			call.Method != "tools/call" {
			next.ServeHTTP(w, r)
			return
		}
		// A tool the store does not serve is unknown, not locked.
		if !IsAccountTool(call.Params.Name) ||
			!Offered(call.Params.Name, t.Features) {
			next.ServeHTTP(w, r)
			return
		}
		identity.WriteChallenge(w, t.Domain,
			`scope="`+linkScopes(t.Features)+`"`,
			"this tool needs a linked account")
	})
}
