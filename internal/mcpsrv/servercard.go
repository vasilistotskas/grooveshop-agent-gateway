package mcpsrv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/net/idna"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/text"
)

// Identity is what an MCP server calls itself, in `initialize` and in its
// Server Card alike. The Server Card extension requires the two not to
// contradict, so both are derived here.
type Identity struct {
	// Name is the reverse-DNS server name the Server Card format
	// requires (namespace/name, e.g. "com.example/store").
	Name string
	// Title is the merchant's store name: on a white-label storefront an
	// agent must be told it is talking to the MERCHANT, not the platform.
	Title string
}

// BridgeIdentity names the in-process server the chat bridge connects
// to; nothing outside the process ever sees it.
var BridgeIdentity = Identity{Name: "grooveshop/agent-gateway", Title: "Storefront"}

// IdentityFor derives a tenant's MCP identity from its primary domain —
// the tenant's stable name, whichever of its hosts the request came in on.
func IdentityFor(t *tenant.Tenant) Identity {
	domain := t.PrimaryDomain
	if domain == "" {
		domain = t.Domain
	}
	title := t.StoreName
	if title == "" {
		title = t.Name
	}
	return Identity{Name: reverseDomain(domain) + "/store", Title: title}
}

func reverseDomain(domain string) string {
	ascii, err := idna.Lookup.ToASCII(strings.TrimSuffix(domain, "."))
	if err != nil {
		ascii = domain
	}
	labels := strings.Split(strings.ToLower(ascii), ".")
	slices.Reverse(labels)
	return strings.Join(labels, ".")
}

// serverCardSchema is the only schema URL a v1 Server Card may declare.
const serverCardSchema = "https://static.modelcontextprotocol.io/schemas/v1/server-card.schema.json"

// serverCard is the SEP-2127 Server Card document: identity and
// connection details only. It deliberately lists no tools — the spec
// excludes primitives, which clients must read from tools/list.
type serverCard struct {
	Schema      string   `json:"$schema"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Title       string   `json:"title,omitempty"`
	WebsiteURL  string   `json:"websiteUrl,omitempty"`
	Icons       []icon   `json:"icons,omitempty"`
	Remotes     []remote `json:"remotes"`
}

type icon struct {
	Src string `json:"src"`
}

type remote struct {
	Type                      string   `json:"type"`
	URL                       string   `json:"url"`
	SupportedProtocolVersions []string `json:"supportedProtocolVersions"`
}

// serverCardDescriptionMax is the format's description bound.
const serverCardDescriptionMax = 100

// ServerCardHandler serves GET <streamable-http-url>/server-card, the
// location the Server Card extension reserves, for the tenant resolved
// from the host. version is the build that also answers `initialize`.
func ServerCardHandler(version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cards hold public metadata only; the extension requires CORS so
		// browser-based clients can read them.
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET")
		h.Set("Access-Control-Allow-Headers", "Content-Type, If-None-Match")
		h.Set("Access-Control-Expose-Headers", "ETag")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		t, ok := tenant.FromContext(r.Context())
		if !ok {
			http.Error(w, "unknown store", http.StatusNotFound)
			return
		}
		id := IdentityFor(t)
		card := serverCard{
			Schema:  serverCardSchema,
			Name:    id.Name,
			Version: version,
			Description: text.Runes("Shopping tools for "+id.Title+
				": catalog, cart, checkout and order tracking.",
				serverCardDescriptionMax),
			Title:      text.Runes(id.Title, serverCardDescriptionMax),
			WebsiteURL: storefront.Home(t.Domain),
			Remotes: []remote{{
				Type:                      "streamable-http",
				URL:                       storefront.MCP(t.Domain),
				SupportedProtocolVersions: mcp.SupportedProtocolVersions(),
			}},
		}
		if t.FaviconURL != "" {
			card.Icons = []icon{{Src: t.FaviconURL}}
		}
		body, err := json.Marshal(card)
		if err != nil {
			http.Error(w, "server card unavailable",
				http.StatusInternalServerError)
			return
		}
		sum := sha256.Sum256(body)
		etag := `"` + hex.EncodeToString(sum[:16]) + `"`
		h.Set("ETag", etag)
		h.Set("Cache-Control", "public, max-age=3600")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.Set("Content-Type", "application/mcp-server-card+json")
		_, _ = w.Write(body)
	})
}
