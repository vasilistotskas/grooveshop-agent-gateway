package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpsig"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// A platform's RFC 9421 signature on /mcp (UCP signatures.md, MCP
// Transport) is verified here, before the MCP handler, which never sees
// the request's method, path or raw body. A request that carries one is
// verified whatever it calls, and refused when it does not verify; the
// platform it proves is put in the request's context (signerFrom).
// A tool that needs a verified platform (signed) refuses a request that
// carries no signature.

type signerKey struct{}

// signerFrom is the platform the request's signature proved: its UCP
// profile URL, normalized (ucp.NormalizeProfileURL); false for an
// unsigned request.
func signerFrom(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(signerKey{}).(string)
	return s, ok
}

// ucpCaller is who asks for a UCP checkout: the platform that signed the
// request, if one did.
func ucpCaller(ctx context.Context) checkout.Caller {
	platform, _ := signerFrom(ctx)
	return checkout.Caller{Protocol: checkout.ProtocolUCP, Platform: platform}
}

// signedTools names the tools that serve only a verified platform, read
// off the toolset (signed).
var signedTools = sync.OnceValue(func() []string {
	var names []string
	for _, t := range definitions() {
		if t.signedOnly {
			names = append(names, t.def.Name)
		}
	}
	return names
})

// ucpSignatureFailed is the JSON-RPC message of every signature refusal,
// as the spec's example words it (signatures.md, MCP Error Response).
const ucpSignatureFailed = "Signature verification failed"

// VerifyPlatformSignature verifies the RFC 9421 signature a request to
// /mcp carries, against the keys of the platform its UCP-Agent header
// names, with created and expires inside window. It answers, itself and
// with the spec's HTTP status and JSON-RPC error, a request that:
//   - carries a signature that does not verify: 401 and -32000
//     (signature_invalid, key_not_found), or 400 and -32600
//     (digest_mismatch, algorithm_unsupported);
//   - names a platform whose profile cannot be had: the discovery
//     failure, as a canonical tool answers it (-32001), or 503 with
//     retry_after when the discovery budget is spent;
//   - is signed and names another platform in a tool's
//     meta.ucp-agent.profile: 401, signature_invalid (Identity Binding);
//   - is not signed and calls a tool that serves only a verified
//     platform: 401, signature_missing.
func VerifyPlatformSignature(
	d Deps,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			signed := len(r.Header.Values("Signature-Input")) > 0 ||
				len(r.Header.Values("Signature")) > 0
			if !signed && r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}
			r, body, ok := readPost(w, r)
			if !ok {
				return
			}
			if !signed {
				if slices.ContainsFunc(body.toolCalls(), func(c calledTool) bool {
					return slices.Contains(signedTools(), c.name)
				}) {
					writeSignatureError(w, body.replyID(), &httpsig.Error{
						Code: httpsig.CodeSignatureMissing,
						Content: "this tool serves a platform by its " +
							"RFC 9421 signature: sign the request with a " +
							"key your UCP profile publishes",
					})
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			signer, err := d.Profiles.VerifyRequest(r.Context(), r, body.raw,
				d.SignatureWindow, time.Now())
			if err == nil {
				err = bindsSigner(body.toolCalls(), signer)
			}
			if err != nil {
				refuseSigned(w, r, d.Log, body.replyID(), err)
				return
			}
			next.ServeHTTP(w, r.WithContext(
				context.WithValue(r.Context(), signerKey{}, signer)))
		})
	}
}

// bindsSigner checks that every tool call that names its platform in
// meta.ucp-agent.profile names the signer: the identity a signature
// proves must be the one the request claims (overview, Identity
// Binding). Arguments are read as the MCP SDK reads them (decodeAsSDK);
// ones it would refuse are left to it. The tools check it again on the
// meta they were given (checkBinding).
func bindsSigner(calls []calledTool, signer string) error {
	for _, c := range calls {
		var in struct {
			Meta *MetaIn `json:"meta"`
		}
		if len(c.args) == 0 || decodeAsSDK(c.args, &in) != nil ||
			in.Meta == nil || in.Meta.UCPAgent == nil {
			continue
		}
		if err := checkBinding(in.Meta.UCPAgent.Profile, signer); err != nil {
			return err
		}
	}
	return nil
}

// errOtherPlatform is a signed request whose meta names a platform other
// than the one that signed it.
var errOtherPlatform = &httpsig.Error{
	Code: httpsig.CodeSignatureInvalid,
	Content: "meta.ucp-agent.profile names another platform than the " +
		"UCP-Agent header the request is signed under",
}

// checkBinding refuses a profile that is not signer's.
func checkBinding(profile, signer string) error {
	if profile == "" {
		return nil
	}
	if p, err := ucp.NormalizeProfileURL(profile); err != nil || p != signer {
		return errOtherPlatform
	}
	return nil
}

// refuseSigned answers a signed request that did not verify.
func refuseSigned(
	w http.ResponseWriter, r *http.Request, log *slog.Logger, id any,
	err error,
) {
	if sigErr, ok := errors.AsType[*httpsig.Error](err); ok {
		writeSignatureError(w, id, sigErr)
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	rpcErr, status := discoveryFailure(err, "")
	if rpcErr == nil {
		log.ErrorContext(r.Context(), "ucp: signature not verified",
			slog.String("error", err.Error()))
		rpcErr = &jsonrpc.Error{Code: jsonrpc.CodeInternalError,
			Message: "Internal error"}
		status = http.StatusInternalServerError
	}
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", strconv.Itoa(ucp.DiscoveryRetryAfter))
	}
	writeRPCError(w, status, id, rpcErr)
}

// writeSignatureError answers with a signature refusal: the code's HTTP
// status and JSON-RPC code (overview, Error Codes, Signature Errors).
func writeSignatureError(w http.ResponseWriter, id any, e *httpsig.Error) {
	status, code := http.StatusUnauthorized, int64(-32000)
	if e.Code == httpsig.CodeDigestMismatch ||
		e.Code == httpsig.CodeAlgorithmUnsupported {
		status, code = http.StatusBadRequest, jsonrpc.CodeInvalidRequest
	}
	data, _ := json.Marshal(map[string]string{
		"code": e.Code, "content": e.Content,
	})
	writeRPCError(w, status, id, &jsonrpc.Error{
		Code: code, Message: ucpSignatureFailed, Data: data,
	})
}

// writeRPCError writes a JSON-RPC error response with an HTTP status:
// on Streamable HTTP the status is the primary signal (overview, Error
// Codes).
func writeRPCError(
	w http.ResponseWriter, status int, id any, e *jsonrpc.Error,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Error   *jsonrpc.Error `json:"error"`
	}{"2.0", id, e})
}
