package httpsig

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dunglas/httpsfv"
)

// The signature error codes UCP defines (signatures.md, Error Handling).
const (
	CodeSignatureMissing     = "signature_missing"
	CodeSignatureInvalid     = "signature_invalid"
	CodeKeyNotFound          = "key_not_found"
	CodeDigestMismatch       = "digest_mismatch"
	CodeAlgorithmUnsupported = "algorithm_unsupported"
)

// Error is a request whose signatures do not authenticate it. Code is
// one of the Code constants; Content says why, for the caller.
type Error struct {
	Code    string
	Content string
}

func (e *Error) Error() string { return "httpsig: " + e.Code + ": " + e.Content }

func refusal(code, format string, args ...any) *Error {
	return &Error{Code: code, Content: fmt.Sprintf(format, args...)}
}

// Key is the public key a signature's keyid names.
type Key struct {
	// Public is an *ecdsa.PublicKey on P-256 or P-384, or an
	// ed25519.PublicKey: the algorithm is derived from it.
	Public crypto.PublicKey
	// Thumbprint is the key's RFC 7638 SHA-256 thumbprint, base64url
	// without padding: a web-bot-auth signature's keyid must equal it.
	Thumbprint string
}

// ErrUnknownKey and ErrUnsupportedKey are what a KeySource answers for
// a keyid that names no key, and for one that names a key this verifier
// cannot use (another key type or curve, or a key not for verifying).
var (
	ErrUnknownKey     = errors.New("httpsig: no such key")
	ErrUnsupportedKey = errors.New("httpsig: key not usable")
)

// KeySource resolves a keyid to the signer's key. Any error other than
// ErrUnknownKey and ErrUnsupportedKey ends the verification with it.
type KeySource func(ctx context.Context, keyID string) (Key, error)

// Window bounds a signature's created and expires parameters, which are
// checked only when present: UCP makes created optional for its default
// signatures (signatures.md, Replay Protection), and RFC 9421 section
// 3.2.1 leaves the window to the application.
type Window struct {
	// MaxAge is how long after its created time a signature verifies.
	MaxAge time.Duration
	// Skew is the clock difference allowed between signer and verifier.
	Skew time.Duration
}

// tagWebBotAuth is the one signature tag UCP accepts beside none: the
// dual-audience Web Bot Auth shape (overview, Identity Resolution
// Algorithm, step 1). A signature with any other tag is skipped.
const tagWebBotAuth = "web-bot-auth"

// maxSignatures is how many of a request's signatures are tried. UCP's
// shapes need one (a dual-audience signature serves UCP and Web Bot
// Auth alike), and RFC 9421 section 4.3 a few for separate audiences;
// each one tried costs a signature check, so the rest are not read.
const maxSignatures = 8

// Verify authenticates req, whose body has been read into body, by the
// signatures it carries (RFC 9421 section 3.2), and returns the label of
// the first one that verifies. Each signature is tried on its own, and
// the request is refused only when none verifies (UCP's Identity
// Resolution Algorithm): the error is the first signature's. Only the
// first maxSignatures are tried.
func Verify(
	ctx context.Context, req *http.Request, body []byte, keys KeySource,
	w Window, now time.Time,
) (string, error) {
	inputs, err := httpsfv.UnmarshalDictionary(req.Header.Values("Signature-Input"))
	if err != nil || len(inputs.Names()) == 0 {
		return "", refusal(CodeSignatureInvalid,
			"Signature-Input is not a dictionary of signatures")
	}
	sigs, err := httpsfv.UnmarshalDictionary(req.Header.Values("Signature"))
	if err != nil {
		return "", refusal(CodeSignatureInvalid,
			"Signature is not a dictionary of signatures")
	}
	b := bodyRead{
		hasBody:  len(body) > 0,
		digested: len(body) == 0 || digestMatches(req, body),
	}
	var first error
	for i, label := range inputs.Names() {
		if i == maxSignatures {
			break
		}
		input, _ := inputs.Get(label)
		err := verifyOne(ctx, req, b, keys, w, now, input, sigs, label)
		if err == nil {
			return label, nil
		}
		if _, refused := errors.AsType[*Error](err); !refused {
			return "", err
		}
		if first == nil {
			first = err
		}
	}
	return "", first
}

// bodyRead is what Verify reads of a request's body once, for each of
// its signatures: whether it has one, and whether Content-Digest
// carries its sha-256.
type bodyRead struct {
	hasBody, digested bool
}

// verifyOne verifies the signature under label.
func verifyOne(
	ctx context.Context, req *http.Request, b bodyRead, keys KeySource,
	w Window, now time.Time, input httpsfv.Member, sigs *httpsfv.Dictionary,
	label string,
) error {
	list, ok := input.(httpsfv.InnerList)
	if !ok {
		return refusal(CodeSignatureInvalid,
			"signature %s: its input is not an inner list", label)
	}
	sig, err := signatureValue(sigs, label)
	if err != nil {
		return err
	}
	keyID, tag, err := checkParams(list.Params, w, now, label)
	if err != nil {
		return err
	}
	covered, err := coveredNames(list.Items, label)
	if err != nil {
		return err
	}
	if missing := uncovered(req, b.hasBody, covered); missing != "" {
		return refusal(CodeSignatureInvalid,
			"signature %s does not cover %s", label, missing)
	}
	key, err := keys(ctx, keyID)
	switch {
	case errors.Is(err, ErrUnknownKey):
		return refusal(CodeKeyNotFound,
			"no key %q in the signer's published keys", keyID)
	case errors.Is(err, ErrUnsupportedKey):
		return refusal(CodeAlgorithmUnsupported,
			"key %q is not one this business verifies with", keyID)
	case err != nil:
		return err
	}
	if tag == tagWebBotAuth && keyID != key.Thumbprint {
		return refusal(CodeSignatureInvalid, "signature %s is tagged %s, "+
			"and its keyid is not its key's RFC 7638 thumbprint",
			label, tagWebBotAuth)
	}
	if !b.digested {
		return refusal(CodeDigestMismatch,
			"Content-Digest does not carry the sha-256 of the body")
	}
	base, err := verifyBase(req, list)
	if err != nil {
		return refusal(CodeSignatureInvalid, "signature %s: %s", label, err)
	}
	return verifyWith(key.Public, []byte(base), sig, label)
}

// signatureValue is the byte sequence the Signature field holds under
// label.
func signatureValue(sigs *httpsfv.Dictionary, label string) ([]byte, error) {
	m, ok := sigs.Get(label)
	if !ok {
		return nil, refusal(CodeSignatureInvalid,
			"Signature has no value for signature %s", label)
	}
	item, ok := m.(httpsfv.Item)
	if !ok {
		return nil, refusal(CodeSignatureInvalid,
			"signature %s is not a byte sequence", label)
	}
	sig, ok := item.Value.([]byte)
	if !ok {
		return nil, refusal(CodeSignatureInvalid,
			"signature %s is not a byte sequence", label)
	}
	return sig, nil
}

// checkParams reads the signature parameters UCP gives meaning to, and
// applies its rules to them: a keyid is required, no alg is sent (the
// algorithm is the key's), the tag is none or web-bot-auth, and created
// and expires, when present, fall inside w.
func checkParams(
	p *httpsfv.Params, w Window, now time.Time, label string,
) (keyID, tag string, err error) {
	if _, ok := p.Get("alg"); ok {
		return "", "", refusal(CodeSignatureInvalid, "signature %s names "+
			"an alg: the algorithm is derived from the key", label)
	}
	if v, ok := p.Get("tag"); ok {
		if tag, _ = v.(string); tag != tagWebBotAuth {
			return "", "", refusal(CodeSignatureInvalid,
				"signature %s carries a tag this business does not "+
					"verify", label)
		}
	}
	v, _ := p.Get("keyid")
	if keyID, _ = v.(string); keyID == "" {
		return "", "", refusal(CodeSignatureInvalid,
			"signature %s names no keyid", label)
	}
	if v, ok := p.Get("created"); ok {
		created, isInt := v.(int64)
		at := time.Unix(created, 0)
		if !isInt || at.After(now.Add(w.Skew)) || now.Sub(at) > w.MaxAge {
			return "", "", refusal(CodeSignatureInvalid,
				"signature %s was not created within the last %s",
				label, w.MaxAge)
		}
	}
	if v, ok := p.Get("expires"); ok {
		expires, isInt := v.(int64)
		if !isInt || now.After(time.Unix(expires, 0).Add(w.Skew)) {
			return "", "", refusal(CodeSignatureInvalid,
				"signature %s has expired", label)
		}
	}
	return keyID, tag, nil
}

// coveredNames is the components the signature covers whole, by name:
// those that carry no parameter. A component with parameters covers
// only part of what its name does (a Dictionary member: RFC 9421 section
// 2.1.2), so it does not count, with one exception: the web-bot-auth
// shape covers signature-agent by the member keyed by the signature's
// own label, which is the member that names its key source (signatures
// .md, WBA Interop, item 3). Each component identifier is a string, and
// occurs once (RFC 9421 section 2.5, step 2.1); its parameters are
// checked when the base is built.
func coveredNames(items []httpsfv.Item, label string) ([]string, error) {
	names := make([]string, 0, len(items))
	seen := make([]string, 0, len(items))
	for _, it := range items {
		name, ok := it.Value.(string)
		if !ok {
			return nil, refusal(CodeSignatureInvalid,
				"signature %s covers a component that is not a string",
				label)
		}
		id, err := httpsfv.Marshal(it)
		if err != nil || slices.Contains(seen, id) {
			return nil, refusal(CodeSignatureInvalid,
				"signature %s names a component twice", label)
		}
		seen = append(seen, id)
		params := it.Params.Names()
		key, _ := it.Params.Get("key")
		switch {
		case len(params) == 0:
			names = append(names, name)
		case name == "signature-agent" && len(params) == 1 && key == label:
			names = append(names, name)
		}
	}
	return names, nil
}

// uncovered names the first component UCP requires a request signature
// to cover that covered lacks, or "" (overview, Identity Resolution
// Algorithm, step 5): the target, the body when there is one, and each
// of ucp-agent, signature-agent and idempotency-key the request carries.
func uncovered(req *http.Request, hasBody bool, covered []string) string {
	required := []string{"@method", "@authority", "@path"}
	if req.URL.RawQuery != "" || req.URL.ForceQuery {
		required = append(required, "@query")
	}
	if hasBody {
		required = append(required, "content-digest", "content-type")
	}
	for _, h := range []string{
		"ucp-agent", "signature-agent", "idempotency-key",
	} {
		if len(req.Header.Values(h)) > 0 {
			required = append(required, h)
		}
	}
	for _, c := range required {
		if !slices.Contains(covered, c) {
			return c
		}
	}
	return ""
}

// digestMatches reports whether the request's Content-Digest holds the
// sha-256 of body, the algorithm UCP requires (RFC 9530).
func digestMatches(req *http.Request, body []byte) bool {
	d, err := httpsfv.UnmarshalDictionary(req.Header.Values("Content-Digest"))
	if err != nil {
		return false
	}
	m, ok := d.Get("sha-256")
	if !ok {
		return false
	}
	item, ok := m.(httpsfv.Item)
	if !ok {
		return false
	}
	got, ok := item.Value.([]byte)
	sum := sha256.Sum256(body)
	return ok && bytes.Equal(got, sum[:])
}

// verifyBase rebuilds the signature base of a received request (RFC 9421
// section 2.5) from the signature's own input. The @signature-params
// line is that input serialized again (section 3.2, step 7).
func verifyBase(req *http.Request, list httpsfv.InnerList) (string, error) {
	var b strings.Builder
	for _, it := range list.Items {
		id, err := httpsfv.Marshal(it)
		if err != nil {
			return "", err
		}
		value, err := receivedValue(req, it)
		if err != nil {
			return "", err
		}
		b.WriteString(id)
		b.WriteString(": ")
		b.WriteString(value)
		b.WriteByte('\n')
	}
	params, err := httpsfv.Marshal(list)
	if err != nil {
		return "", err
	}
	b.WriteString(`"@signature-params": `)
	b.WriteString(params)
	return b.String(), nil
}

// receivedValue is a covered component's value. The one parameter it
// takes is key, which selects a Dictionary field's member (RFC 9421
// section 2.1.2), as the web-bot-auth shape covers signature-agent; any
// other is not understood, and fails the signature (section 2.5).
func receivedValue(req *http.Request, it httpsfv.Item) (string, error) {
	name, _ := it.Value.(string)
	params := it.Params.Names()
	if len(params) == 0 {
		return componentValue(req, name)
	}
	v, _ := it.Params.Get("key")
	key, isString := v.(string)
	if len(params) > 1 || !isString || strings.HasPrefix(name, "@") {
		return "", fmt.Errorf("component %q has parameters this "+
			"verifier does not understand", name)
	}
	d, err := httpsfv.UnmarshalDictionary(req.Header.Values(name))
	if err != nil {
		return "", fmt.Errorf("%q is not a dictionary", name)
	}
	m, ok := d.Get(key)
	if !ok {
		return "", fmt.Errorf("%q has no member %q", name, key)
	}
	sv, ok := m.(httpsfv.StructuredFieldValue)
	if !ok {
		return "", fmt.Errorf("%q member %q cannot be serialized", name, key)
	}
	return httpsfv.Marshal(sv)
}

// verifyWith checks sig over base with the key's algorithm: ECDSA as the
// fixed-width r||s RFC 9421 section 3.3.4 requires (64 bytes for P-256,
// 96 for P-384), Ed25519 as RFC 8032 encodes it.
func verifyWith(key crypto.PublicKey, base, sig []byte, label string) error {
	ok := false
	switch k := key.(type) {
	case ed25519.PublicKey:
		ok = len(sig) == ed25519.SignatureSize && ed25519.Verify(k, base, sig)
	case *ecdsa.PublicKey:
		var digest []byte
		switch k.Curve {
		case elliptic.P256():
			sum := sha256.Sum256(base)
			digest = sum[:]
		case elliptic.P384():
			sum := sha512.Sum384(base)
			digest = sum[:]
		default:
			return refusal(CodeAlgorithmUnsupported,
				"signature %s is made with a curve this business does "+
					"not verify", label)
		}
		size := (k.Curve.Params().BitSize + 7) / 8
		if len(sig) == 2*size {
			r := new(big.Int).SetBytes(sig[:size])
			s := new(big.Int).SetBytes(sig[size:])
			ok = ecdsa.Verify(k, digest, r, s)
		}
	default:
		return refusal(CodeAlgorithmUnsupported,
			"signature %s is made with a key this business does not "+
				"verify", label)
	}
	if !ok {
		return refusal(CodeSignatureInvalid,
			"signature %s does not verify", label)
	}
	return nil
}
