package mcpsrv

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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
	ctx := context.Background()
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
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
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

// The checkout tools publish the spec's own request schemas: update and
// complete take them as they are, create takes them with the
// gateway's cart_id beside `checkout` as the other way to name the lines.
func TestCheckoutToolsPublishTheSpecsRequestSchemas(t *testing.T) {
	listed, err := ucpSession(t).ListTools(context.Background(), nil)
	require.NoError(t, err)
	schemas := map[string]map[string]any{}
	for _, tool := range listed.Tools {
		raw, err := json.Marshal(tool.InputSchema)
		require.NoError(t, err)
		var s map[string]any
		require.NoError(t, json.Unmarshal(raw, &s))
		schemas[tool.Name] = s
	}
	checkoutOf := func(tool string) string {
		raw, err := json.Marshal(schemas[tool]["properties"].(map[string]any)["checkout"])
		require.NoError(t, err)
		return string(raw)
	}
	assert.JSONEq(t, string(ucp.CheckoutRequestSchema(ucp.OpUpdate)),
		checkoutOf("update_checkout"))
	assert.JSONEq(t, string(ucp.CheckoutRequestSchema(ucp.OpComplete)),
		checkoutOf("complete_checkout"))
	assert.Contains(t, checkoutOf("create_checkout"),
		`"$id":"https://ucp.dev/schemas/shopping/checkout.json"`)
	assert.Contains(t, checkoutOf("create_checkout"), `"discounts"`)

	for _, tool := range []string{"create_checkout", "get_checkout",
		"update_checkout", "complete_checkout", "cancel_checkout",
		"get_order"} {
		meta := schemas[tool]["properties"].(map[string]any)["meta"]
		assert.Equal(t, true,
			meta.(map[string]any)["additionalProperties"],
			"%s: the spec lets a platform add metadata", tool)
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
// with a type the field takes, or is listed here as one the schema leaves
// open: the gateway's own members, outside the negotiated shape.
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
		own    []string
	}{
		{"create_checkout", createCheckoutInput,
			reflect.TypeFor[CreateCheckoutIn](),
			[]string{"checkout.fulfillment", "checkout.pay_way_id"}},
		{"update_checkout", updateCheckoutInput,
			reflect.TypeFor[UpdateCheckoutIn](),
			[]string{"checkout.fulfillment", "checkout.pay_way_id"}},
		{"complete_checkout", completeCheckoutInput,
			reflect.TypeFor[CompleteCheckoutIn](),
			[]string{"checkout.pay_way_id"}},
		{"get_checkout", getCheckoutInput,
			reflect.TypeFor[GetCheckoutIn](), nil},
		{"cancel_checkout", cancelCheckoutInput,
			reflect.TypeFor[CancelCheckoutIn](), nil},
		{"get_order", getOrderInput, reflect.TypeFor[GetOrderIn](), nil},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			c := structCheck{t: t, instrument: &instrument, own: tc.own}
			c.object("", tc.in, tc.schema)
		})
	}
}

type structCheck struct {
	t          *testing.T
	instrument *jsonschema.Schema
	own        []string
}

// branches is s and every schema it is the allOf of, with the bundle's
// self-reference read as the instrument.
func (c structCheck) branches(s *jsonschema.Schema) []*jsonschema.Schema {
	if s.Ref == "#" {
		s = c.instrument
	}
	out := []*jsonschema.Schema{s}
	for _, b := range s.AllOf {
		out = append(out, c.branches(b)...)
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
	for i := range typ.NumField() {
		f := typ.Field(i)
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
		if prop == nil {
			assert.Contains(c.t, c.own, member,
				"%s is not in the schema, so the schema takes any value", member)
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
