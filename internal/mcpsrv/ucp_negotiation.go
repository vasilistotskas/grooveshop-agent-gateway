package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// CheckoutResult is the OpenRPC checkout_result: the checkout session,
// or an error_response when no valid resource can be established.
type CheckoutResult struct {
	checkout *ucp.Checkout
	failure  *ucp.ErrorResponse
}

func (r CheckoutResult) MarshalJSON() ([]byte, error) {
	if r.failure != nil {
		return json.Marshal(r.failure)
	}
	return json.Marshal(r.checkout)
}

// OrderResult is the OpenRPC order_result: the order, or an
// error_response.
type OrderResult struct {
	order   *ucp.Order
	failure *ucp.ErrorResponse
}

func (r OrderResult) MarshalJSON() ([]byte, error) {
	if r.failure != nil {
		return json.Marshal(r.failure)
	}
	return json.Marshal(r.order)
}

// resultSchema is the tool output schema for a canonical result: one of
// the resource or the error response, as the OpenRPC document declares.
// The union types marshal themselves, so the schema cannot be inferred.
func resultSchema[Resource any]() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		OneOf: []*jsonschema.Schema{
			mustSchema(reflect.TypeFor[Resource]()),
			mustSchema(reflect.TypeFor[ucp.ErrorResponse]()),
		},
	}
}

func mustSchema(t reflect.Type) *jsonschema.Schema {
	s, err := jsonschema.ForType(t, &jsonschema.ForOptions{})
	if err != nil {
		panic("mcpsrv: output schema for " + t.String() + ": " + err.Error())
	}
	return s
}

// The canonical tools' output schemas: built once, they are identical
// for every tenant's server.
var (
	checkoutResultSchema = resultSchema[ucp.Checkout]()
	orderResultSchema    = resultSchema[ucp.Order]()
)

// ucpDiscoveryFailed is the spec's JSON-RPC code for a profile that could
// not be fetched, validated or version-matched.
const ucpDiscoveryFailed = -32001

// negotiate resolves the calling platform's profile and intersects its
// capabilities with this business's.
//
// A discovery failure is the one place a canonical tool returns a
// protocol error rather than a tool result: the spec defines it as a
// transport error (-32001), not a business outcome. The spec also asks
// for the matching HTTP status on Streamable HTTP; the SDK answers every
// JSON-RPC error with 200, so the JSON-RPC code is the only signal. continueURL is the
// web handoff for the operation.
func (h *handlers) negotiate(
	ctx context.Context, t *tenant.Tenant, meta *MetaIn, continueURL string,
) (ucp.Negotiated, error) {
	var zero ucp.Negotiated
	p, err := h.deps.Profiles.Resolve(ctx, meta.UCPAgent.Profile)
	if err == nil {
		err = ucp.CheckVersion(p)
	}
	var neg ucp.Negotiated
	if err == nil {
		neg = ucp.Negotiate(ucp.BusinessCapabilities(t), p)
		// The webhook endpoint is called on every order transition, so
		// an uncallable one is refused before anything is stored.
		if hook := neg.OrderWebhookURL(); hook != "" {
			if verr := ucp.ValidatePlatformURL(
				hook, h.deps.AllowLocalWebhooks); verr != nil {
				err = &ucp.DiscoveryError{
					Code: ucp.CodeProfileMalformed,
					Content: "the order capability's webhook_url is not " +
						"a callable endpoint: " + verr.Error(),
				}
			}
		}
	}
	if errors.Is(err, ucp.ErrDiscoveryBusy) {
		// The spec's retryable 503: -32000 with data.retry_after.
		data, _ := json.Marshal(map[string]int{
			"retry_after": ucp.DiscoveryRetryAfter,
		})
		return zero, &jsonrpc.Error{
			Code: -32000, Message: "Service temporarily unavailable",
			Data: data,
		}
	}
	if err != nil {
		derr, ok := errors.AsType[*ucp.DiscoveryError](err)
		if !ok {
			return zero, err
		}
		message := "UCP discovery failed"
		if derr.Code == ucp.CodeVersionUnsupported {
			message = "Protocol version not supported"
		}
		data, _ := json.Marshal(map[string]string{
			"code":         derr.Code,
			"content":      derr.Content,
			"continue_url": continueURL,
		})
		return zero, &jsonrpc.Error{
			Code: ucpDiscoveryFailed, Message: message, Data: data,
		}
	}
	return neg, nil
}
