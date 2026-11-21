package mcpsrv

import (
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// ucpSession is a client of a store's server whose handlers have no
// store: a call the input schema accepts reaches the handler and is
// answered "missing store context", one it refuses never gets there.
func ucpSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := t.Context()
	serverT, clientT := mcp.NewInMemoryTransports()
	_, err := NewServer(Deps{Version: "test", Log: slog.New(slog.DiscardHandler)},
		Identity{Name: "test.acme/store", Title: "Acme Store"}, EveryFeature).
		Connect(ctx, serverT, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(ctx, clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// accepted reports whether the server took args for tool: decoded them
// and called the handler.
func accepted(
	t *testing.T, session *mcp.ClientSession, tool, args string,
) (bool, string) {
	t.Helper()
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: tool, Arguments: json.RawMessage(args),
	})
	require.NoError(t, err)
	require.True(t, res.IsError)
	text := res.Content[0].(*mcp.TextContent).Text
	return strings.Contains(text, "missing store context"), text
}

const specMeta = `{"ucp-agent":{"profile":` +
	`"https://platform.example/profiles/shopping-agent.json"}}`

const specIdempotentMeta = `{"ucp-agent":{"profile":"https://..."},` +
	`"idempotency-key":"550e8400-e29b-41d4-a716-446655440000"}`

// ucpSchemas are the input schemas a store's tools/list carries, by tool.
func ucpSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()
	return listedSchemas(t, NewServer(Deps{Version: "test"},
		Identity{Name: "test.acme/store", Title: "Acme Store"}, EveryFeature))
}

// checkoutParts are the two schemas a tool's `checkout` is the allOf of:
// the spec's request schema, and the business's own members.
func checkoutParts(
	t *testing.T, schema map[string]any,
) (spec, own map[string]any) {
	t.Helper()
	checkout := schema["properties"].(map[string]any)["checkout"]
	parts, _ := checkout.(map[string]any)["allOf"].([]any)
	require.Len(t, parts, 2, "checkout is not the spec's schema and the "+
		"business's own members")
	return parts[0].(map[string]any), parts[1].(map[string]any)
}

// The checkout tools publish the spec's own request schemas, as generated:
// update and complete take them as they are, create takes them with the
// gateway's cart_id beside `checkout` as the other way to name the lines.
func TestCheckoutToolsPublishTheSpecsRequestSchemas(t *testing.T) {
	schemas := ucpSchemas(t)
	specOf := func(tool string) string {
		spec, _ := checkoutParts(t, schemas[tool])
		raw, err := json.Marshal(spec)
		require.NoError(t, err)
		return string(raw)
	}
	// As the SDK's schema type holds it, which drops an empty
	// `required` (the fulfillment extension's shipping destination has
	// one): the same schema.
	generated := func(op ucp.Operation) string {
		var s jsonschema.Schema
		require.NoError(t, json.Unmarshal(ucp.CheckoutRequestSchema(op), &s))
		raw, err := json.Marshal(&s)
		require.NoError(t, err)
		return string(raw)
	}
	assert.JSONEq(t, generated(ucp.OpUpdate), specOf("update_checkout"))
	assert.JSONEq(t, generated(ucp.OpComplete), specOf("complete_checkout"))
	assert.Contains(t, specOf("create_checkout"),
		`"$id":"https://ucp.dev/schemas/shopping/checkout.json"`)
	assert.Contains(t, specOf("create_checkout"), `"discounts"`)

	for _, tool := range []string{"create_checkout", "get_checkout",
		"update_checkout", "complete_checkout", "cancel_checkout",
		"get_order"} {
		meta := schemas[tool]["properties"].(map[string]any)["meta"]
		assert.Equal(t, true,
			meta.(map[string]any)["additionalProperties"],
			"%s: the spec lets a platform add metadata", tool)
	}
}

// The business's own member in `checkout`, `pay_way_id`, is described
// beside the spec's members on every operation that takes a payment; the
// delivery is the spec's fulfillment extension, with nothing of the
// business's own.
func TestCheckoutToolsDescribeTheBusinessesOwnMembers(t *testing.T) {
	schemas := ucpSchemas(t)
	for _, tool := range []string{"create_checkout", "update_checkout",
		"complete_checkout"} {
		t.Run(tool, func(t *testing.T) {
			spec, own := checkoutParts(t, schemas[tool])
			members := own["properties"].(map[string]any)
			assert.Equal(t, []string{"pay_way_id"},
				slices.Collect(maps.Keys(members)))
			assert.Contains(t,
				members["pay_way_id"].(map[string]any)["description"],
				ucp.HostedSelectionCapability)
			raw, err := json.Marshal(spec)
			require.NoError(t, err)
			if tool != "complete_checkout" {
				assert.Contains(t, string(raw),
					`"$id":"https://ucp.dev/schemas/shopping/types/fulfillment.json"`)
			}
		})
	}
}

// A delivery is checked as the fulfillment extension's request schema
// has it: a destination of a shipping method is a postal address, a
// method needs its line items, and an update may name a method by the
// id the business gave it instead of its type.
func TestFulfillmentIsCheckedAsTheExtensionHasIt(t *testing.T) {
	session := ucpSession(t)
	call := func(fulfillment string) (bool, string) {
		return accepted(t, session, "update_checkout", `{"meta":`+specMeta+
			`,"id":"c","checkout":{"line_items":[{"item":{"id":"5"},`+
			`"quantity":1}],"fulfillment":`+fulfillment+`}}`)
	}
	destination := `{"street_address":"Πανεπιστημίου 12",` +
		`"extended_address":"3ος όροφος","address_locality":"Αθήνα",` +
		`"postal_code":"10431","address_country":"GR"}`
	for name, tc := range map[string]struct {
		fulfillment string
		taken       bool
	}{
		"a shipping destination and an option": {`{"methods":[{"type":` +
			`"shipping","line_item_ids":["410"],"destinations":[` +
			destination + `],"groups":[{"id":"parcel",` +
			`"selected_option_id":"acs"}]}]}`, true},
		"a destination's member mistyped": {`{"methods":[{"type":` +
			`"shipping","line_item_ids":[],"destinations":[` +
			`{"postal_code":10431}]}]}`, false},
		"a method without its line items": {`{"methods":[{"type":` +
			`"shipping"}]}`, false},
		"no methods": {`{}`, true},
		"a method named by its id, without its type": {`{"methods":[` +
			`{"id":"shipping","line_item_ids":["410"],` +
			`"selected_destination_id":"shipping_address","groups":[` +
			`{"id":"parcel","selected_option_id":"acs"}]}]}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			ok, text := call(tc.fulfillment)
			assert.Equal(t, tc.taken, ok, text)
		})
	}
}

// Every request example of the spec's MCP checkout binding and of the
// discount extension's page is accepted. The binding's examples elide
// their lines and payment ("[ ... ]", "{...}"), which are filled in with
// the members the spec's schemas describe.
func TestSpecRequestExamplesAreAccepted(t *testing.T) {
	session := ucpSession(t)
	line := `{"item":{"id":"prod_1"},"quantity":2}`
	for name, tc := range map[string]struct{ tool, args string }{
		"mcp.md request metadata": {"create_checkout", `{"meta":` +
			specIdempotentMeta + `,"checkout":{"line_items":[` + line + `]}}`},
		"mcp.md create_checkout": {"create_checkout", `{"meta":` + specMeta +
			`,"checkout":{"buyer":{"email":"jane.doe@example.com",` +
			`"first_name":"Jane","last_name":"Doe"},"line_items":[{"id":` +
			`"li_1","item":{"id":"item_123"},"quantity":1}],"currency":` +
			`"USD","fulfillment":{"methods":[{"type":"shipping",` +
			`"destinations":[{"street_address":"123 Main St",` +
			`"address_locality":"Springfield","address_region":"IL",` +
			`"postal_code":"62701","address_country":"US"}]}]}}}`},
		"mcp.md update_checkout": {"update_checkout", `{"meta":` + specMeta +
			`,"id":"checkout_abc123","checkout":{"buyer":{"email":` +
			`"jane.doe@example.com","first_name":"Jane","last_name":` +
			`"Doe"},"line_items":[{"item":{"id":"item_123"},"id":"li_1",` +
			`"quantity":1}],"currency":"USD","fulfillment":{"methods":[` +
			`{"id":"shipping_1","line_item_ids":["li_1"],"groups":[{"id":` +
			`"package_1","selected_option_id":"express"}]}]}}}`},
		"mcp.md complete_checkout": {"complete_checkout", `{"meta":` +
			specIdempotentMeta + `,"id":"checkout_abc123","checkout":` +
			`{"payment":{"instruments":[{"id":"pi_1","handler_id":` +
			`"handler_1","type":"cash_on_delivery","selected":true}]}}}`},
		"mcp.md get_checkout": {"get_checkout",
			`{"meta":` + specMeta + `,"id":"checkout_abc123"}`},
		"mcp.md cancel_checkout": {"cancel_checkout", `{"meta":` +
			specIdempotentMeta + `,"id":"checkout_abc123"}`},
		"discount.md codes": {"create_checkout", `{"meta":` + specMeta +
			`,"checkout":{"line_items":[` + line + `],"discounts":` +
			`{"codes":["SUMMER20"]}}}`},
		"discount.md order-level discount": {"update_checkout", `{"meta":` +
			specMeta + `,"id":"c","checkout":{"line_items":[` + line +
			`],"discounts":{"codes":["SAVE10"]}}}`},
		"discount.md rejected code": {"update_checkout", `{"meta":` +
			specMeta + `,"id":"c","checkout":{"line_items":[` + line +
			`],"discounts":{"codes":["SAVE10","EXPIRED50"]}}}`},
		"discount.md clear codes": {"update_checkout", `{"meta":` +
			specMeta + `,"id":"c","checkout":{"line_items":[` + line +
			`],"discounts":{"codes":[]}}}`},
		"discount.md eligibility claim": {"create_checkout", `{"meta":` +
			specMeta + `,"checkout":{"context":{"eligibility":` +
			`["com.example.store_card"]},"line_items":[{"item":{"id":` +
			`"prod_shirt"},"quantity":2}]}}`},
	} {
		t.Run(name, func(t *testing.T) {
			ok, text := accepted(t, session, tc.tool, tc.args)
			assert.True(t, ok, text)
		})
	}
}

// Members no handler reads are accepted at every level and dropped when
// the arguments are decoded, among them a whole response sent back.
func TestUnknownMembersAreAcceptedAndIgnored(t *testing.T) {
	args := `{"meta":{"ucp-agent":{"profile":"https://p.example/p.json",` +
		`"x":1},"trace":"t-1"},"extra":true,"checkout":{"ucp":{"version":` +
		`"2026-08-25"},"id":"chk","status":"incomplete","totals":[],` +
		`"links":[],"signals":{"dev.ucp.buyer_ip":"203.0.113.9"},` +
		`"attribution":{"utm_source":"agent"},"line_items":[{"id":"li_1",` +
		`"item":{"id":"5","title":"Mug","price":900,"x":{}},"quantity":` +
		`2,"totals":[{"type":"total","amount":1800}],"parent_id":"p"}],` +
		`"buyer":{"first_name":"Jane","nickname":"J"},"discounts":{` +
		`"codes":["SAVE10"],"applied":[{"code":"SAVE10","title":"10%",` +
		`"amount":180}],"x":1},"payment":{"instruments":[{"id":"pi",` +
		`"handler_id":"h","type":"cash_on_delivery","display":` +
		`{"brand":"x"},"x":1}],"x":1},"x":{"y":[1]}}}`
	ok, text := accepted(t, ucpSession(t), "create_checkout", args)
	require.True(t, ok, text)

	var in CreateCheckoutIn
	require.NoError(t, json.Unmarshal([]byte(args), &in))
	assert.Equal(t, "https://p.example/p.json", in.Meta.UCPAgent.Profile)
	assert.Equal(t, []UCPLineItemReqIn{{Item: UCPItemIn{ID: "5"},
		Quantity: 2}}, in.Checkout.LineItems)
	assert.Equal(t, &UCPBuyerReqIn{FirstName: "Jane"}, in.Checkout.Buyer)
	assert.Equal(t, []string{"SAVE10"}, in.Checkout.Discounts.Codes)
	assert.Equal(t, []UCPInstrumentIn{{HandlerID: "h",
		Type: "cash_on_delivery"}}, in.Checkout.Payment.Instruments)
}

// What the spec's schemas refuse is refused before the handler: a create
// with neither lines nor a cart, an update without its lines, a complete
// without its payment or its key, a line with no quantity.
func TestWhatTheSpecRefusesIsRefused(t *testing.T) {
	session := ucpSession(t)
	line := `{"item":{"id":"5"},"quantity":1}`
	for name, tc := range map[string]struct{ tool, args string }{
		"create naming no lines": {"create_checkout",
			`{"meta":` + specMeta + `,"checkout":{}}`},
		"update without lines": {"update_checkout",
			`{"meta":` + specMeta + `,"id":"c","checkout":{"buyer":{}}}`},
		"complete without payment": {"complete_checkout",
			`{"meta":` + specIdempotentMeta + `,"id":"c","checkout":{}}`},
		"complete without a key": {"complete_checkout", `{"meta":` +
			specMeta + `,"id":"c","checkout":{"payment":{}}}`},
		"a line without its quantity": {"create_checkout", `{"meta":` +
			specMeta + `,"checkout":{"line_items":[{"item":{"id":"5"}}]}}`},
		"a quantity below one": {"update_checkout", `{"meta":` + specMeta +
			`,"id":"c","checkout":{"line_items":[{"item":{"id":"5"},` +
			`"quantity":0}]}}`},
		"meta without the agent": {"get_checkout",
			`{"meta":{},"id":"c"}`},
	} {
		t.Run(name, func(t *testing.T) {
			ok, text := accepted(t, session, tc.tool, tc.args)
			assert.False(t, ok, text)
			assert.Contains(t, text, `validating "arguments"`)
		})
	}

	// A cart is the other way to name the lines.
	ok, text := accepted(t, session, "create_checkout", `{"meta":`+specMeta+
		`,"cart_id":"`+fixtureCartID+`","checkout":{}}`)
	assert.True(t, ok, text)
	ok, text = accepted(t, session, "update_checkout", `{"meta":`+specMeta+
		`,"id":"c","checkout":{"line_items":[`+line+`]}}`)
	assert.True(t, ok, text)
}

// The SDK validates the arguments against the published schema and then
// decodes them into the handler's struct, which it never compares with
// the schema. Every member a struct decodes is described by its schema
// with a type the field takes: the spec's members by the spec's schema
// (a shipping method's destinations under its type's conditional
// branch), the business's own (cart_id, pay_way_id) by its own.
func TestRequestStructsDecodeWhatTheSchemasAccept(t *testing.T) {
	// ucp-schema bundles an instrument's {"$ref": "#"} so that it points
	// at the Payment object, not at the instrument: the members are
	// checked against the vendored payment_instrument.json instead.
	raw, err := os.ReadFile(filepath.Join("..", "ucp", "spec", ucp.Version,
		"common", "types", "payment_instrument.json"))
	require.NoError(t, err)
	var instrument jsonschema.Schema
	require.NoError(t, json.Unmarshal(raw, &instrument))

	for _, tc := range []struct {
		tool   string
		schema *jsonschema.Schema
		in     reflect.Type
	}{
		{"create_checkout", createCheckoutInput,
			reflect.TypeFor[CreateCheckoutIn]()},
		{"update_checkout", updateCheckoutInput,
			reflect.TypeFor[UpdateCheckoutIn]()},
		{"complete_checkout", completeCheckoutInput,
			reflect.TypeFor[CompleteCheckoutIn]()},
		{"get_checkout", getCheckoutInput,
			reflect.TypeFor[GetCheckoutIn]()},
		{"cancel_checkout", cancelCheckoutInput,
			reflect.TypeFor[CancelCheckoutIn]()},
		{"get_order", getOrderInput, reflect.TypeFor[GetOrderIn]()},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			c := structCheck{t: t, instrument: &instrument,
				omitted: omittedBy[tc.tool]}
			c.object("", tc.in, tc.schema)
		})
	}
}

// omittedBy is, per tool, the members a struct shares with another tool
// that this tool's schema omits (ucp_request "omit"). A create names one
// method, which is the chosen one, so it omits the method's line items
// and its id, which the business gives: delivery refuses any id but the
// business's, whichever tool decodes it.
var omittedBy = map[string]map[string]bool{
	"create_checkout": {
		"checkout.fulfillment.methods[].id":            true,
		"checkout.fulfillment.methods[].line_item_ids": true,
	},
}

type structCheck struct {
	t          *testing.T
	instrument *jsonschema.Schema
	// omitted are members the operation's schema leaves out that its
	// struct shares with another operation's (omittedBy).
	omitted map[string]bool
}

// branches is s and every schema it is the allOf of, and the then of
// each conditional among them, with the bundle's self-reference read as
// the instrument.
func (c structCheck) branches(s *jsonschema.Schema) []*jsonschema.Schema {
	if s.Ref == "#" {
		s = c.instrument
	}
	out := []*jsonschema.Schema{s}
	for _, b := range s.AllOf {
		out = append(out, c.branches(b)...)
	}
	if s.Then != nil {
		out = append(out, c.branches(s.Then)...)
	}
	return out
}

func (c structCheck) types(s *jsonschema.Schema) []string {
	var out []string
	for _, b := range c.branches(s) {
		if b.Type != "" {
			out = append(out, b.Type)
		}
		out = append(out, b.Types...)
	}
	return out
}

func (c structCheck) object(path string, typ reflect.Type, s *jsonschema.Schema) {
	for f := range typ.Fields() {
		if f.Anonymous {
			c.object(path, f.Type, s)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		member := strings.TrimPrefix(path+"."+name, ".")
		var prop *jsonschema.Schema
		for _, b := range c.branches(s) {
			if p, ok := b.Properties[name]; ok {
				prop = p
				break
			}
		}
		if c.omitted[member] {
			assert.Nil(c.t, prop, "%s is in the schema now", member)
			continue
		}
		if !assert.NotNil(c.t, prop,
			"%s is not in the schema, so the schema takes any value", member) {
			continue
		}
		c.value(member, f.Type, prop)
	}
}

func (c structCheck) value(member string, typ reflect.Type, s *jsonschema.Schema) {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	want := map[reflect.Kind]string{
		reflect.String: "string", reflect.Int: "integer",
		reflect.Int64: "integer", reflect.Bool: "boolean",
		reflect.Slice: "array", reflect.Struct: "object",
	}[typ.Kind()]
	require.NotEmpty(c.t, want, "%s: a %s field", member, typ)
	assert.Contains(c.t, c.types(s), want, member)
	switch typ.Kind() {
	case reflect.Slice:
		var items *jsonschema.Schema
		for _, b := range c.branches(s) {
			if b.Items != nil {
				items = b.Items
			}
		}
		require.NotNil(c.t, items, "%s: no item schema", member)
		c.value(member+"[]", typ.Elem(), items)
	case reflect.Struct:
		c.object(member, typ, s)
	}
}
