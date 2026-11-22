package mcpsrv

import (
	"encoding/json"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// The canonical UCP tools' input schemas. Each is the tool's params from
// the service's OpenRPC document: `meta`, `id` for an existing resource,
// and `checkout`, the spec's own request schema for the operation
// (ucp.CheckoutRequestSchema). So a tool accepts what the spec accepts.
// The spec's schemas are open: a member no handler reads is dropped when
// the arguments are decoded, never refused. The business's own members
// are described too, beside the spec's: `cart_id` on create, and
// `pay_way_id` in `checkout`.
//
// The SDK validates the arguments against these schemas and decodes them
// into the handlers' request structs, which it does not compare with the
// schemas: TestRequestStructsDecodeWhatTheSchemasAccept does.
var (
	createCheckoutInput   = createInput()
	getCheckoutInput      = resourceInput("the checkout session id", false)
	updateCheckoutInput   = checkoutInput(ucp.OpUpdate, "the checkout session id to update")
	completeCheckoutInput = checkoutInput(ucp.OpComplete, "the checkout session id to place")
	cancelCheckoutInput   = resourceInput("the checkout session id to cancel", true)
	getOrderInput         = resourceInput("the order id from the completed checkout", false)
)

// checkoutSchemaID is the base checkout object's id inside the bundled
// request schemas.
const checkoutSchemaID = "https://ucp.dev/schemas/shopping/checkout.json"

// metaSchema is the OpenRPC document's `meta`. complete_checkout and
// cancel_checkout require the idempotency key, as the document does; its
// form is checked by MetaIn.validate, which takes more than a UUID.
func metaSchema(idempotent bool) *jsonschema.Schema {
	required := []string{"ucp-agent"}
	if idempotent {
		required = append(required, "idempotency-key")
	}
	return &jsonschema.Schema{
		Type:                 "object",
		Description:          "Request metadata.",
		Required:             required,
		AdditionalProperties: &jsonschema.Schema{},
		Properties: map[string]*jsonschema.Schema{
			"ucp-agent": {
				Type: "object",
				Description: "Platform agent identification, for capability " +
					"negotiation. Maps to the HTTP UCP-Agent header.",
				Required: []string{"profile"},
				Properties: map[string]*jsonschema.Schema{
					"profile": {
						Type: "string", Format: "uri",
						Description: "URL of the calling platform's UCP " +
							"profile document.",
					},
				},
			},
			"idempotency-key": {
				Type: "string",
				Description: "Unique key for retry safety: a UUID, or up " +
					"to 255 visible ASCII characters. Repeat it to retry.",
			},
		},
	}
}

// resourceInput is {meta, id}: an operation on an existing resource that
// sends no body.
func resourceInput(id string, idempotent bool) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:     "object",
		Required: []string{"meta", "id"},
		Properties: map[string]*jsonschema.Schema{
			"meta": metaSchema(idempotent),
			"id":   {Type: "string", Description: id},
		},
	}
}

// checkoutInput is {meta, id, checkout} for update and complete.
func checkoutInput(op ucp.Operation, id string) *jsonschema.Schema {
	s := resourceInput(id, op == ucp.OpComplete)
	s.Required = append(s.Required, "checkout")
	s.Properties["checkout"] = checkoutRequest(op)
	return s
}

// createInput is {meta, checkout}, and the gateway's own `cart_id` beside
// `checkout`, outside the negotiated shape (CreateCheckoutIn.CartID). The
// spec requires line_items on create; a create naming a cart sends none,
// so the schema requires one or the other.
func createInput() *jsonschema.Schema {
	checkout := checkoutRequest(ucp.OpCreate)
	if !dropRequired(checkout, checkoutSchemaID, "line_items") {
		panic("mcpsrv: the create request schema requires no line_items")
	}
	return &jsonschema.Schema{
		Type:     "object",
		Required: []string{"meta", "checkout"},
		Properties: map[string]*jsonschema.Schema{
			"meta":     metaSchema(false),
			"checkout": checkout,
			"cart_id": {
				Type: "string",
				Description: "the cartId of an existing cart from the cart " +
					"tools; omit when sending checkout.line_items. A cart " +
					"has one checkout at a time: an open checkout that " +
					"already holds the cart is canceled, and this one " +
					"replaces it",
			},
		},
		AnyOf: []*jsonschema.Schema{
			{Required: []string{"cart_id"}},
			{Properties: map[string]*jsonschema.Schema{
				"checkout": {Required: []string{"line_items"}},
			}},
		},
	}
}

// checkoutRequest is the `checkout` of op: the spec's request schema, and
// the business's own members composed onto it the way an extension
// composes its own (allOf), so that tools/list describes them. The spec's
// schema is used as generated; only what is the business's is added here.
func checkoutRequest(op ucp.Operation) *jsonschema.Schema {
	var spec jsonschema.Schema
	if err := json.Unmarshal(
		ucp.CheckoutRequestSchema(op), &spec); err != nil {
		panic("mcpsrv: request schema for " + string(op) + ": " + err.Error())
	}
	own := map[string]*jsonschema.Schema{"pay_way_id": {
		Type: "integer",
		Description: "The business's own member, of its declared " +
			"extension " + ucp.HostedSelectionCapability + ": selects " +
			"an ONLINE payment method by its id (see " +
			"get_payment_methods), which the buyer then pays on the " +
			"store's hosted page. Accepted only while that extension " +
			"is negotiated, refused otherwise. Omit it and use " +
			"payment.instruments for a method the checkout advertises.",
	}}
	return &jsonschema.Schema{AllOf: []*jsonschema.Schema{&spec, {
		Type:       "object",
		Title:      "The business's own checkout members",
		Properties: own,
	}}}
}

// dropRequired removes member from the required list of every schema
// with id among s and its allOf branches (each extension composes its
// own copy of the checkout), reporting whether any required it.
func dropRequired(s *jsonschema.Schema, id, member string) bool {
	dropped := false
	if s.ID == id && slices.Contains(s.Required, member) {
		s.Required = slices.DeleteFunc(s.Required,
			func(m string) bool { return m == member })
		dropped = true
	}
	for _, branch := range s.AllOf {
		if dropRequired(branch, id, member) {
			dropped = true
		}
	}
	return dropped
}
