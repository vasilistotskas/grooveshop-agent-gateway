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
	title := t.StoreName
	if title == "" {
		title = t.Name
	}
	labels := strings.Split(publisher(t), ".")
	slices.Reverse(labels)
	// Bounded here, not in the card, so initialize reports the same title.
	return Identity{
		Name:  strings.Join(labels, ".") + "/store",
		Title: text.Runes(title, serverCardDescriptionMax),
	}
}

// Description is the summary the Server Card gives, and the discovery
// entries repeat word for word.
func (id Identity) Description() string {
	return text.Runes("Shopping tools for "+id.Title+
		": catalog, cart, checkout and order tracking.",
		serverCardDescriptionMax)
}

// publisher is the tenant's primary domain as a lower-case ASCII FQDN:
// the stem of its reverse-DNS server name and the authority segment of
// its discovery identifier, which admits no IDN form.
func publisher(t *tenant.Tenant) string {
	domain := t.PrimaryDomain
	if domain == "" {
		domain = t.Domain
	}
	ascii, err := idna.Lookup.ToASCII(strings.TrimSuffix(domain, "."))
	if err != nil {
		ascii = domain
	}
	return strings.ToLower(ascii)
}

// serverCardSchema is the only schema URL a v1 Server Card may declare.
const serverCardSchema = "https://static.modelcontextprotocol.io/schemas/v1/server-card.schema.json"

// serverCardMediaType names a Server Card: its Content-Type, and the
// `type` of every discovery entry that points at one.
const serverCardMediaType = "application/mcp-server-card+json"

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
	return publicDocument(serverCardMediaType, func(t *tenant.Tenant) any {
		id := IdentityFor(t)
		card := serverCard{
			Schema:      serverCardSchema,
			Name:        id.Name,
			Version:     version,
			Description: id.Description(),
			Title:       id.Title,
			WebsiteURL:  storefront.Home(t.Domain),
			Remotes: []remote{{
				Type:                      "streamable-http",
				URL:                       storefront.MCP(t.Domain),
				SupportedProtocolVersions: mcp.SupportedProtocolVersions(),
			}},
		}
		if t.FaviconURL != "" {
			card.Icons = []icon{{Src: t.FaviconURL}}
		}
		return card
	})
}

// publicDocument serves the metadata document build makes for the
// tenant resolved from the host. It is public, so it is readable
// cross-origin (the Server Card extension requires CORS for browser
// clients) and cacheable, revalidated by ETag.
func publicDocument(
	contentType string, build func(*tenant.Tenant) any,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		body, err := json.Marshal(build(t))
		if err != nil {
			http.Error(w, "document unavailable",
				http.StatusInternalServerError)
			return
		}
		sum := sha256.Sum256(body)
		etag := `"` + hex.EncodeToString(sum[:16]) + `"`
		h.Set("ETag", etag)
		h.Set("Cache-Control", "public, max-age=3600")
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.Set("Content-Type", contentType)
		_, _ = w.Write(body)
	})
}

// etagMatches applies If-None-Match's weak comparison (RFC 9110 13.1.2):
// a list of tags, W/ prefixes ignored, or "*".
func etagMatches(header, etag string) bool {
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}
