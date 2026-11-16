package ucp

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/dunglas/httpsfv"
	"golang.org/x/net/idna"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpsig"
)

// JWK is a public key a platform profile publishes in its top-level
// keys[] (profile.json jwk_public_key), which the profile's schema has
// validated before it is decoded into this.
type JWK struct {
	Kid    string   `json:"kid"`
	Kty    string   `json:"kty"`
	Crv    string   `json:"crv"`
	X      string   `json:"x"`
	Y      string   `json:"y"`
	Use    string   `json:"use"`
	KeyOps []string `json:"key_ops"`
}

// forVerifying reports whether the key may verify a signature: not one
// marked for encryption, nor one whose key_ops leave verify out (RFC
// 7517 sections 4.2 and 4.3; overview, Identity Resolution Algorithm,
// step 4).
func (k JWK) forVerifying() bool {
	return k.Use != "enc" &&
		(k.KeyOps == nil || slices.Contains(k.KeyOps, "verify"))
}

// verifyKey is the key in the form httpsig verifies with: an ECDSA key
// on P-256 or P-384, or an Ed25519 key. Any other is
// httpsig.ErrUnsupportedKey: the kty and crv vocabularies are open, and
// a key this business cannot use spoils only the signatures it made.
func (k JWK) verifyKey() (httpsig.Key, error) {
	x, err := base64.RawURLEncoding.Strict().DecodeString(k.X)
	if err != nil {
		return httpsig.Key{}, httpsig.ErrUnsupportedKey
	}
	switch {
	case k.Kty == "OKP" && k.Crv == "Ed25519" &&
		len(x) == ed25519.PublicKeySize:
		return httpsig.Key{
			Public: ed25519.PublicKey(x),
			Thumbprint: thumbprint(map[string]string{
				"crv": k.Crv, "kty": k.Kty, "x": k.X,
			}),
		}, nil
	case k.Kty == "EC":
		curve, ok := map[string]elliptic.Curve{
			"P-256": elliptic.P256(), "P-384": elliptic.P384(),
		}[k.Crv]
		y, err := base64.RawURLEncoding.Strict().DecodeString(k.Y)
		if !ok || err != nil {
			return httpsig.Key{}, httpsig.ErrUnsupportedKey
		}
		size := (curve.Params().BitSize + 7) / 8
		if len(x) != size || len(y) != size {
			return httpsig.Key{}, httpsig.ErrUnsupportedKey
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(curve,
			append(append([]byte{4}, x...), y...))
		if err != nil {
			return httpsig.Key{}, httpsig.ErrUnsupportedKey
		}
		return httpsig.Key{
			Public: pub,
			Thumbprint: thumbprint(map[string]string{
				"crv": k.Crv, "kty": k.Kty, "x": k.X, "y": k.Y,
			}),
		}, nil
	}
	return httpsig.Key{}, httpsig.ErrUnsupportedKey
}

// key is the signature-capable key the profile publishes under kid.
// A key that does not decode as a JWK (a member of another type than
// this business reads, key_ops that is not an array) is skipped like
// one not for verifying.
func (p *PlatformProfile) key(kid string) (JWK, bool) {
	for _, raw := range p.Keys {
		var k JWK
		if json.Unmarshal(raw, &k) == nil && k.Kid == kid &&
			k.forVerifying() {
			return k, true
		}
	}
	return JWK{}, false
}

// NormalizeProfileURL is the one spelling of a profile URL that platform
// identities are compared in: the scheme and the host lowercased, the
// host as its IDNA A-label, the scheme's default port dropped, an empty
// path as "/" (RFC 3986 section 6.2.3), and no fragment, which is never
// fetched. The path and the query are kept as sent.
func NormalizeProfileURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" ||
		(u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("%w: not an http(s) URL", ErrPlatformURL)
	}
	host := strings.ToLower(u.Hostname())
	if addr, perr := netip.ParseAddr(host); perr == nil && addr.Is6() {
		host = "[" + host + "]"
	} else if perr != nil {
		if host, err = idna.Lookup.ToASCII(host); err != nil {
			return "", fmt.Errorf("%w: not a host name", ErrPlatformURL)
		}
	}
	scheme := strings.ToLower(u.Scheme)
	defaultPort := map[string]string{"https": "443", "http": "80"}[scheme]
	if port := u.Port(); port != "" && port != defaultPort {
		host += ":" + port
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	out := scheme + "://" + host + path
	if u.RawQuery != "" || u.ForceQuery {
		out += "?" + u.RawQuery
	}
	return out, nil
}

// errNoUCPAgent is a signed request that does not name its signer: UCP
// resolves the signing key through the UCP-Agent profile.
var errNoUCPAgent = &httpsig.Error{
	Code: httpsig.CodeSignatureInvalid,
	Content: "a signed request names its signer's profile in the " +
		`UCP-Agent header, as profile="<url>"`,
}

// ucpAgentProfile reads the profile URL of a UCP-Agent header, an RFC
// 8941 Dictionary whose profile member is a String (signatures.md,
// UCP-Agent parsing rules). The URL itself is checked where it is
// fetched (ProfileResolver.Resolve).
func ucpAgentProfile(values []string) (string, error) {
	d, err := httpsfv.UnmarshalDictionary(values)
	if err != nil {
		return "", errNoUCPAgent
	}
	m, ok := d.Get("profile")
	if !ok {
		return "", errNoUCPAgent
	}
	item, ok := m.(httpsfv.Item)
	if !ok {
		return "", errNoUCPAgent
	}
	profile, ok := item.Value.(string)
	if !ok || profile == "" {
		return "", errNoUCPAgent
	}
	return profile, nil
}

// VerifyRequest authenticates a platform's request by its RFC 9421
// signatures, with the keys its UCP-Agent profile publishes, and returns
// that profile's URL normalized (NormalizeProfileURL): the platform the
// request is from. body is the request's body, already read.
//
// A signature whose keyid the profile does not publish has the profile
// fetched again, once per request and at most once a TTL floor per
// origin (Refresh), since the platform may have rotated its key.
//
// A refusal is an *httpsig.Error; a profile that cannot be had is a
// *DiscoveryError or ErrDiscoveryBusy, as Resolve answers.
func (r *ProfileResolver) VerifyRequest(
	ctx context.Context, req *http.Request, body []byte, w httpsig.Window,
	now time.Time,
) (string, error) {
	profileURL, err := ucpAgentProfile(req.Header.Values("UCP-Agent"))
	if err != nil {
		return "", err
	}
	signer, err := NormalizeProfileURL(profileURL)
	if err != nil {
		return "", discoveryErr(CodeInvalidProfileURL, "%s", err.Error())
	}
	p, err := r.Resolve(ctx, profileURL)
	if err != nil {
		return "", err
	}
	refreshed := false
	keys := func(ctx context.Context, kid string) (httpsig.Key, error) {
		if k, ok := p.key(kid); ok {
			return k.verifyKey()
		}
		if refreshed {
			return httpsig.Key{}, httpsig.ErrUnknownKey
		}
		refreshed = true
		fresh, err := r.Refresh(ctx, profileURL)
		switch {
		case errors.Is(err, errRefreshSpent):
			return httpsig.Key{}, httpsig.ErrUnknownKey
		case err != nil:
			// The profile cannot be had now: the discovery failure, as
			// Resolve answers it, never a key the platform lacks.
			return httpsig.Key{}, err
		}
		p = fresh
		if k, ok := p.key(kid); ok {
			return k.verifyKey()
		}
		return httpsig.Key{}, httpsig.ErrUnknownKey
	}
	if _, err := httpsig.Verify(ctx, req, body, keys, w, now); err != nil {
		return "", err
	}
	return signer, nil
}

// SignRequest signs a platform's request to a business as UCP asks of
// its HTTP transports (signatures.md, REST Request Signing and MCP
// Transport): UCP-Agent names profileURL, Content-Digest binds body, and
// one signature, made with key and created at now, covers the target,
// the body and every header UCP requires covered (overview, Identity
// Resolution Algorithm, step 5). body is the request's body, already
// read. It is the platform's side of VerifyRequest, for the tests and
// for cmd/ucp-sign, which signs requests to a running gateway.
func SignRequest(
	req *http.Request, body []byte, profileURL string, key *SigningKey,
	now time.Time,
) error {
	agent := httpsfv.NewDictionary()
	agent.Add("profile", httpsfv.NewItem(profileURL))
	header, err := httpsfv.Marshal(agent)
	if err != nil {
		return fmt.Errorf("ucp: the profile URL is not a header string: %w",
			err)
	}
	req.Header.Set("UCP-Agent", header)
	components := []string{"@method", "@authority", "@path"}
	if req.URL.RawQuery != "" || req.URL.ForceQuery {
		components = append(components, "@query")
	}
	components = append(components, "ucp-agent")
	for _, h := range []string{"signature-agent", "idempotency-key"} {
		if len(req.Header.Values(h)) > 0 {
			components = append(components, h)
		}
	}
	if len(body) > 0 {
		req.Header.Set("Content-Digest", httpsig.ContentDigest(body))
		components = append(components, "content-digest", "content-type")
	}
	return httpsig.Sign(req, "sig1", components,
		httpsig.Params{Created: now.Unix()}, key)
}
