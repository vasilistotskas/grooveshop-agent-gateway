// Package httpsig signs outgoing HTTP requests per RFC 9421 (HTTP Message
// Signatures) and computes RFC 9530 Content-Digest values — the REST
// binding UCP mandates for webhooks. It implements the subset the gateway
// signs with: the @method, @authority, @path and @query derived
// components, plain header fields, and the created and keyid parameters.
package httpsig

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Signer produces the raw signature over a signature base. The encoding is
// the algorithm's own (RFC 9421 section 3.3): 64 bytes of r||s for ES256,
// the RFC 8032 encoding for Ed25519.
type Signer interface {
	KeyID() string
	Sign(base []byte) ([]byte, error)
}

// Params are the signature parameters. Created is a Unix time; zero omits
// it. The algorithm is never sent: verifiers derive it from the key.
type Params struct {
	Created int64
}

// ContentDigest is the RFC 9530 sha-256 Content-Digest header value.
func ContentDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
}

// Sign covers components of req and sets the Signature-Input and
// Signature headers under label. Header components must already be set
// on req, since the base reads their final values.
func Sign(
	req *http.Request, label string, components []string, p Params,
	signer Signer,
) error {
	params := signatureParams(components, p, signer.KeyID())
	base, err := signatureBase(req, components, params)
	if err != nil {
		return err
	}
	sig, err := signer.Sign([]byte(base))
	if err != nil {
		return fmt.Errorf("httpsig: sign: %w", err)
	}
	req.Header.Set("Signature-Input", label+"="+params)
	req.Header.Set("Signature",
		label+"=:"+base64.StdEncoding.EncodeToString(sig)+":")
	return nil
}

// signatureParams serializes the @signature-params value: the inner list
// of covered components followed by the parameters.
func signatureParams(components []string, p Params, keyID string) string {
	var b strings.Builder
	b.WriteByte('(')
	for i, c := range components {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.Quote(c))
	}
	b.WriteByte(')')
	if p.Created != 0 {
		b.WriteString(";created=")
		b.WriteString(strconv.FormatInt(p.Created, 10))
	}
	b.WriteString(`;keyid=`)
	b.WriteString(strconv.Quote(keyID))
	return b.String()
}

// signatureBase builds the RFC 9421 section 2.5 signature base.
func signatureBase(
	req *http.Request, components []string, params string,
) (string, error) {
	var b strings.Builder
	for _, c := range components {
		value, err := componentValue(req, c)
		if err != nil {
			return "", err
		}
		b.WriteString(strconv.Quote(c))
		b.WriteString(": ")
		b.WriteString(value)
		b.WriteByte('\n')
	}
	b.WriteString(`"@signature-params": `)
	b.WriteString(params)
	return b.String(), nil
}

func componentValue(req *http.Request, name string) (string, error) {
	switch name {
	case "@method":
		return req.Method, nil
	case "@authority":
		return authority(req), nil
	case "@path":
		if p := req.URL.EscapedPath(); p != "" {
			return p, nil
		}
		return "/", nil
	case "@query":
		return "?" + req.URL.RawQuery, nil
	}
	if strings.HasPrefix(name, "@") {
		return "", fmt.Errorf("httpsig: unsupported component %q", name)
	}
	// Values aliases the header's own slice: trim into a copy.
	values := req.Header.Values(name)
	if len(values) == 0 {
		return "", fmt.Errorf("httpsig: covered header %q is not set", name)
	}
	trimmed := make([]string, len(values))
	for i, v := range values {
		trimmed[i] = strings.TrimSpace(v)
	}
	return strings.Join(trimmed, ", "), nil
}

// authority is the target's lowercased host, with the port only when it
// is not the scheme's default (RFC 9421 section 2.2.3).
func authority(req *http.Request) string {
	host := strings.ToLower(req.URL.Host)
	switch {
	case req.URL.Scheme == "https" && strings.HasSuffix(host, ":443"):
		return strings.TrimSuffix(host, ":443")
	case req.URL.Scheme == "http" && strings.HasSuffix(host, ":80"):
		return strings.TrimSuffix(host, ":80")
	}
	return host
}
