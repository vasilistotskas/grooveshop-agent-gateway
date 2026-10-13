package django

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The consumer contract is the OpenAPI document of what this gateway needs
// from Django: every endpoint it calls, the query parameters and body it
// sends, and only the response properties its DTOs read. The
// infrastructure deploy gate diffs it against a Django release's
// schema.yml (`oasdiff breaking --flatten-allof --severity-levels
// contract/oasdiff-levels.txt`), so a Django change that drops or loosens
// a field the gateway decodes is refused instead of silently zero-filled.
//
// It is generated from the DTO types below, never edited by hand:
//
//	go test ./internal/django -run TestDjangoConsumerContract -update

var update = flag.Bool("update", false, "rewrite "+contractFile)

const (
	contractFile = "../../contract/django-consumer.openapi.json"
	// djangoAPIPrefix is the API root DJANGO_BASE_URL points at; the
	// client's request paths are relative to it.
	djangoAPIPrefix = "/api/v1"
)

// contractCall is one Django call made by a Client method.
type contractCall struct {
	fn     string // the Client method making the call
	method string
	path   string // request.path: relative to djangoAPIPrefix, templated
	// status is the success status Django documents; 0 when it documents
	// none (the gateway then discards the body anyway).
	status int
	query  []contractParam
	body   reflect.Type // nil: no request body
	out    reflect.Type // nil: response body discarded
	// names maps a Go type (generic arguments dropped) to the component
	// name Django's schema.yml gives that shape, so the contract and
	// schema.yml read alike. Types not named here are inlined.
	names map[string]string
}

// contractParam is a query parameter; required means the call sends it
// every time.
type contractParam struct {
	name     string
	required bool
}

// outsideOpenAPI lists Client methods calling Django endpoints that its
// OpenAPI document does not describe, with the reason.
var outsideOpenAPI = map[string]string{
	"Ping": "/health/live is the liveness probe, not part of the API schema",
}

var djangoContract = []contractCall{
	{
		fn: "ResolveTenant", method: http.MethodGet,
		path: "/tenant/resolve", status: http.StatusOK,
		query: []contractParam{{"domain", true}},
		out:   reflect.TypeFor[TenantConfig](),
		names: map[string]string{"TenantConfig": "TenantConfig"},
	},
	{
		fn: "AgentMe", method: http.MethodGet,
		path: "/agent/me", status: http.StatusOK,
		out:   reflect.TypeFor[AgentProfile](),
		names: map[string]string{"AgentProfile": "AgentProfile"},
	},
	{
		fn: "AgentOrders", method: http.MethodGet,
		path: "/agent/me/orders", status: http.StatusOK,
		out:   reflect.TypeFor[[]AgentOrder](),
		names: map[string]string{"AgentOrder": "Order"},
	},
	{
		fn: "AgentLoyalty", method: http.MethodGet,
		path: "/agent/me/loyalty", status: http.StatusOK,
		out:   reflect.TypeFor[AgentLoyalty](),
		names: map[string]string{"AgentLoyalty": "LoyaltySummary"},
	},
	{
		fn: "AgentFavourites", method: http.MethodGet,
		path: "/agent/me/favourites", status: http.StatusOK,
		out:   reflect.TypeFor[[]AgentFavourite](),
		names: map[string]string{"AgentFavourite": "AgentFavourite"},
	},
	{
		fn: "SearchProducts", method: http.MethodGet,
		path: "/search/product", status: http.StatusOK,
		query: []contractParam{
			{"query", true}, {"languageCode", false},
			{"categories", false}, {"priceMin", false},
			{"priceMax", false}, {"sort", false}, {"facets", false},
			{"limit", false}, {"offset", false},
		},
		out: reflect.TypeFor[SearchProductResponse](),
		names: map[string]string{
			"SearchProductResponse": "ProductMeiliSearchResponse",
		},
	},
	{
		fn: "Trending", method: http.MethodGet,
		path: "/search/trending", status: http.StatusOK,
		query: []contractParam{
			{"contentType", true}, {"languageCode", false},
			{"limit", false},
		},
		out: reflect.TypeFor[TrendingResponse](),
		names: map[string]string{
			"TrendingResponse": "TrendingSearchResponse",
		},
	},
	{
		fn: "GetProduct", method: http.MethodGet,
		path: "/product/{id}", status: http.StatusOK,
		out:   reflect.TypeFor[ProductDetail](),
		names: map[string]string{"ProductDetail": "ProductRetrieve"},
	},
	{
		fn: "GetProductVariants", method: http.MethodGet,
		path: "/product/{id}/variants", status: http.StatusOK,
		out: reflect.TypeFor[ProductVariants](),
		names: map[string]string{
			"ProductVariants": "ProductVariantsResponse",
			"ProductVariant":  "ProductVariant",
		},
	},
	{
		fn: "GetProductReviews", method: http.MethodGet,
		path: "/product/{id}/reviews", status: http.StatusOK,
		query: []contractParam{{"pageSize", false}},
		out:   reflect.TypeFor[Page[Review]](),
		names: map[string]string{
			"Page": "PaginatedProductReviewList", "Review": "ProductReview",
		},
	},
	{
		fn: "ListAllCategories", method: http.MethodGet,
		path: "/product/category/all", status: http.StatusOK,
		out:   reflect.TypeFor[[]Category](),
		names: map[string]string{"Category": "ProductCategory"},
	},
	{
		fn: "ListProductsPage", method: http.MethodGet,
		path: "/product", status: http.StatusOK,
		query: []contractParam{{"page", true}, {"pageSize", true}},
		out:   reflect.TypeFor[Page[Product]](),
		names: map[string]string{
			"Page": "PaginatedProductList", "Product": "Product",
		},
	},
	{
		fn: "PayWays", method: http.MethodGet,
		path: "/pay_way", status: http.StatusOK,
		query: []contractParam{
			{"pageSize", true}, {"shippingProviderCode", false},
			{"shippingKind", false},
		},
		out: reflect.TypeFor[Page[PayWay]](),
		names: map[string]string{
			"Page": "PaginatedPayWayList", "PayWay": "PayWay",
		},
	},
	{
		fn: "PayWayByID", method: http.MethodGet,
		path: "/pay_way/{id}", status: http.StatusOK,
		out:   reflect.TypeFor[PayWay](),
		names: map[string]string{"PayWay": "PayWay"},
	},
	{
		fn: "GetCart", method: http.MethodGet,
		path: "/cart", status: http.StatusOK,
		out:   reflect.TypeFor[Cart](),
		names: map[string]string{"Cart": "CartDetail"},
	},
	{
		fn: "AddCartItem", method: http.MethodPost,
		path: "/cart/item", status: http.StatusCreated,
		body: reflect.TypeFor[cartItemCreate](),
		out:  reflect.TypeFor[CartItem](),
		names: map[string]string{
			"cartItemCreate": "CartItemCreateRequest",
			"CartItem":       "CartItemDetail",
		},
	},
	{
		fn: "UpdateCartItem", method: http.MethodPut,
		path: "/cart/item/{id}", status: http.StatusOK,
		body: reflect.TypeFor[cartItemUpdate](),
		out:  reflect.TypeFor[CartItem](),
		names: map[string]string{
			"cartItemUpdate": "CartItemUpdateRequest",
			"CartItem":       "CartItemDetail",
		},
	},
	{
		fn: "RemoveCartItem", method: http.MethodDelete,
		path: "/cart/item/{id}",
	},
	{
		fn: "ApplyCoupon", method: http.MethodPost,
		path: "/cart/coupon", status: http.StatusOK,
		body: reflect.TypeFor[couponApply](),
		out:  reflect.TypeFor[Cart](),
		names: map[string]string{
			"couponApply": "CouponApplyRequestRequest",
			"Cart":        "CartDetail",
		},
	},
	{
		fn: "RemoveCoupon", method: http.MethodDelete,
		path: "/cart/coupon", status: http.StatusOK,
		out:   reflect.TypeFor[Cart](),
		names: map[string]string{"Cart": "CartDetail"},
	},
	{
		fn: "ReserveStock", method: http.MethodPost,
		path: "/cart/reserve-stock", status: http.StatusOK,
		out: reflect.TypeFor[ReserveStockResult](),
		names: map[string]string{
			"ReserveStockResult": "ReserveStockResponse",
		},
	},
	{
		fn: "CreateProductAlert", method: http.MethodPost,
		path: "/product/alert", status: http.StatusCreated,
		body: reflect.TypeFor[productAlertCreate](),
		out:  reflect.TypeFor[ProductAlert](),
		names: map[string]string{
			"productAlertCreate": "ProductAlertRequest",
			"ProductAlert":       "ProductAlert",
		},
	},
	{
		fn: "OrderByUUID", method: http.MethodGet,
		path: "/order/uuid/{uuid}", status: http.StatusOK,
		out:   reflect.TypeFor[Order](),
		names: map[string]string{"Order": "OrderDetail"},
	},
	{
		fn: "CreateOrder", method: http.MethodPost,
		path: "/order", status: http.StatusCreated,
		body: reflect.TypeFor[OrderCreate](),
		out:  reflect.TypeFor[Order](),
		names: map[string]string{
			"OrderCreate": "OrderCreateFromCartRequest",
			"Order":       "OrderDetail",
		},
	},
	{
		fn: "CreateOrderCheckoutSession", method: http.MethodPost,
		path:   "/order/{id}/create_checkout_session",
		status: http.StatusOK,
		query:  []contractParam{{"uuid", true}},
		body:   reflect.TypeFor[checkoutSessionCreate](),
		out:    reflect.TypeFor[CheckoutSessionResult](),
		names: map[string]string{
			"checkoutSessionCreate": "CreateCheckoutSessionRequestRequest",
			"CheckoutSessionResult": "CreateCheckoutSessionResponse",
		},
	},
	{
		fn: "ShippingOptions", method: http.MethodGet,
		path: "/shipping/options", status: http.StatusOK,
		query: []contractParam{
			{"countryCode", true}, {"orderValueAmount", false},
			{"currency", false}, {"weightGrams", false},
		},
		out:   reflect.TypeFor[[]ShippingOption](),
		names: map[string]string{"ShippingOption": "ShippingOption"},
	},
	{
		fn: "FreeShippingInfo", method: http.MethodGet,
		path: "/shipping/free-shipping-info", status: http.StatusOK,
		query: []contractParam{{"countryCode", true}},
		out:   reflect.TypeFor[FreeShippingInfo](),
		names: map[string]string{"FreeShippingInfo": "FreeShippingInfo"},
	},
	{
		fn: "AcsNearestStations", method: http.MethodGet,
		path: "/shipping/acs/stations/nearest", status: http.StatusOK,
		query: []contractParam{{"postalCode", true}, {"city", false}},
		out:   reflect.TypeFor[[]AcsStation](),
		names: map[string]string{"AcsStation": "AcsStation"},
	},
	{
		fn: "BoxNowNearestLocker", method: http.MethodPost,
		path: "/shipping/boxnow/lockers/nearest", status: http.StatusOK,
		body: reflect.TypeFor[BoxNowNearestRequest](),
		out:  reflect.TypeFor[BoxNowLocker](),
		names: map[string]string{
			"BoxNowNearestRequest": "BoxNowNearestLockerRequestRequest",
			"BoxNowLocker":         "BoxNowNearestLockerResponse",
		},
	},
}

func TestDjangoConsumerContract(t *testing.T) {
	got := buildContract(t)
	if *update {
		require.NoError(t, os.MkdirAll(filepath.Dir(contractFile), 0o755))
		require.NoError(t, os.WriteFile(contractFile, got, 0o600))
		return
	}
	want, err := os.ReadFile(contractFile)
	require.NoError(t, err, "missing contract: run with -update")
	if line, diff := firstDiff(want, got); diff != "" {
		t.Fatalf("%s is stale at line %d (a DTO or the call registry "+
			"changed):\n%s\nRegenerate it and commit the result:\n"+
			"\tgo test ./internal/django -run TestDjangoConsumerContract "+
			"-update", contractFile, line, diff)
	}
}

func firstDiff(want, got []byte) (int, string) {
	w := strings.Split(string(want), "\n")
	g := strings.Split(string(got), "\n")
	for i := range max(len(w), len(g)) {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return i + 1, fmt.Sprintf("\tcommitted: %q\n\tgenerated: %q",
				wl, gl)
		}
	}
	return 0, ""
}

var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

func buildContract(t *testing.T) []byte {
	t.Helper()
	g := &schemaGen{
		t:          t,
		components: map[string]any{},
		owners:     map[string]reflect.Type{},
	}
	paths := map[string]map[string]any{}
	for _, c := range djangoContract {
		g.names = c.names
		op := map[string]any{"summary": "django.Client." + c.fn}

		var params []any
		for _, m := range pathParam.FindAllStringSubmatch(c.path, -1) {
			params = append(params, parameter(m[1], "path", true))
		}
		for _, q := range c.query {
			params = append(params, parameter(q.name, "query", q.required))
		}
		if len(params) > 0 {
			op["parameters"] = params
		}

		if c.body != nil {
			op["requestBody"] = map[string]any{
				"required": true,
				"content":  jsonContent(g.topLevel(c.body)),
			}
		}

		resp := map[string]any{}
		key := "default"
		if c.status != 0 {
			key = strconv.Itoa(c.status)
			resp["description"] = http.StatusText(c.status)
		} else {
			resp["description"] = "Django documents no success response; " +
				"the gateway discards the body"
		}
		if c.out != nil {
			resp["content"] = jsonContent(g.topLevel(c.out))
		}
		op["responses"] = map[string]any{key: resp}

		path := djangoAPIPrefix + c.path
		if paths[path] == nil {
			paths[path] = map[string]any{}
		}
		method := strings.ToLower(c.method)
		require.NotContains(t, paths[path], method,
			"%s %s registered twice", c.method, c.path)
		paths[path][method] = op
	}

	doc := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "grooveshop-agent-gateway: Django API consumer contract",
			"version": "1.0.0",
			"description": "What the agent gateway calls on the Django API " +
				"and the response properties it reads. GENERATED by " +
				"TestDjangoConsumerContract (internal/django/" +
				"contract_test.go) from the Go DTOs; do not edit.",
		},
		"paths":      paths,
		"components": map[string]any{"schemas": g.components},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)
	return append(raw, '\n')
}

// parameter declares a query or path parameter. Its schema is a string:
// that is what goes on the wire, and Django's integer parameters accept
// it.
func parameter(name, in string, required bool) map[string]any {
	return map[string]any{
		"name":     name,
		"in":       in,
		"required": required,
		"schema":   map[string]any{"type": "string"},
	}
}

func jsonContent(schema map[string]any) map[string]any {
	return map[string]any{
		"application/json": map[string]any{"schema": schema},
	}
}

var (
	jsonNumberType = reflect.TypeFor[json.Number]()
	rawMessageType = reflect.TypeFor[json.RawMessage]()
)

// schemaGen derives OpenAPI schemas from DTO types. A property is
// required unless its field is a pointer, omitempty or tagged
// `contract:"optional"`, and nullable when its field is a pointer
// without omitempty or tagged `contract:"nullable"`. The tags record
// where Django's schema is looser than the Go type: decoding an absent
// field or a null into a value type leaves its zero value, which the
// tagged field's readers treat as "not set". The other tags:
//
//   - untyped: Django declares a oneOf union, which oasdiff reports as a
//     type change against any plain type; the contract asserts presence.
//   - undocumented: read, but missing from Django's OpenAPI document.
type schemaGen struct {
	t          *testing.T
	names      map[string]string
	components map[string]any
	owners     map[string]reflect.Type
}

// topLevel is a request or response body: its struct must be named after
// the Django component it maps to.
func (g *schemaGen) topLevel(t reflect.Type) map[string]any {
	elem := t
	for elem.Kind() == reflect.Slice {
		elem = elem.Elem()
	}
	if _, ok := g.names[goTypeName(elem)]; !ok {
		g.t.Fatalf("%s: name its Django component in the registry", elem)
	}
	return g.schema(t)
}

func goTypeName(t reflect.Type) string {
	name, _, _ := strings.Cut(t.Name(), "[")
	return name
}

func (g *schemaGen) schema(t reflect.Type) map[string]any {
	switch t {
	case jsonNumberType:
		return map[string]any{"type": "number"}
	case rawMessageType:
		// Forwarded verbatim: the gateway reads none of its members.
		return map[string]any{}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
		reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16,
		reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": g.schema(t.Elem())}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			g.t.Fatalf("%s: JSON object keys must be strings", t)
		}
		return map[string]any{
			"type":                 "object",
			"additionalProperties": g.schema(t.Elem()),
		}
	case reflect.Pointer:
		// Nullability is the field's (see fields).
		return g.schema(t.Elem())
	case reflect.Struct:
		if name, ok := g.names[goTypeName(t)]; ok {
			return g.ref(name, t)
		}
		return g.object(t)
	default:
		g.t.Fatalf("%s: no OpenAPI mapping for kind %s", t, t.Kind())
		return nil
	}
}

func (g *schemaGen) nullable(t reflect.Type, s map[string]any) {
	if _, isRef := s["$ref"]; isRef {
		// OpenAPI 3.0 ignores siblings of $ref.
		g.t.Fatalf("%s: a nullable field cannot be a named component", t)
	}
	s["nullable"] = true
}

func (g *schemaGen) ref(name string, t reflect.Type) map[string]any {
	if owner, seen := g.owners[name]; seen {
		if owner != t {
			g.t.Fatalf("component %s maps to both %s and %s", name, owner, t)
		}
	} else {
		g.owners[name] = t
		g.components[name] = g.object(t)
	}
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func (g *schemaGen) object(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string
	g.fields(t, props, &required)
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		slices.Sort(required)
		s["required"] = required
	}
	return s
}

func (g *schemaGen) fields(
	t reflect.Type, props map[string]any, required *[]string,
) {
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			g.fields(f.Type, props, required)
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			g.t.Fatalf("%s.%s: decoded fields need a json tag", t, f.Name)
		}

		pointer := f.Type.Kind() == reflect.Pointer
		omitempty := slices.Contains(strings.Split(opts, ","), "omitempty")
		optional := pointer || omitempty
		// A pointer that is omitted when nil is never sent as null.
		nullable := pointer && !omitempty
		untyped, undocumented := false, false
		if ct := f.Tag.Get("contract"); ct != "" {
			for v := range strings.SplitSeq(ct, ",") {
				switch v {
				case "optional":
					optional = true
				case "nullable":
					nullable = true
				case "untyped":
					untyped = true
				case "undocumented":
					undocumented = true
				default:
					g.t.Fatalf("%s.%s: unknown contract tag %q",
						t, f.Name, v)
				}
			}
		}
		if undocumented {
			// oasdiff could only ever report it removed.
			continue
		}

		s := map[string]any{}
		if !untyped {
			s = g.schema(f.Type)
		}
		if nullable {
			g.nullable(f.Type, s)
		}
		props[name] = s
		if !optional {
			*required = append(*required, name)
		}
	}
}

// TestDjangoContractCoversEveryCall keeps the registry honest: every
// Django call a Client method makes must be registered with the same
// method, path, query parameters and body/response types the code uses,
// so a new call cannot ship without its contract.
func TestDjangoContractCoversEveryCall(t *testing.T) {
	observed := observeClientCalls(t)

	registered := map[string]bool{}
	for _, c := range djangoContract {
		registered[c.fn] = true
		obs, ok := observed[c.fn]
		if !assert.Truef(t, ok, "registry entry %s: Client.%s makes no "+
			"Django call", c.path, c.fn) {
			continue
		}
		want := observedCall{
			method: c.method,
			path:   c.path,
			body:   goTypeString(c.body),
			out:    goTypeString(c.out),
			query:  map[string]bool{},
		}
		for _, q := range c.query {
			want.query[q.name] = q.required
		}
		assert.Equalf(t, want, obs, "Client.%s: registry entry differs "+
			"from the call in the code", c.fn)
	}
	for fn, reason := range outsideOpenAPI {
		assert.Containsf(t, observed, fn, "outsideOpenAPI: %s", reason)
		assert.Falsef(t, registered[fn], "%s is both registered and "+
			"outside the OpenAPI document", fn)
	}
	for fn, obs := range observed {
		_, outside := outsideOpenAPI[fn]
		assert.Truef(t, registered[fn] || outside, "Client.%s calls %s "+
			"%s but is not in djangoContract: register it so the "+
			"consumer contract covers it", fn, obs.method, obs.path)
	}
}

// goTypeString renders t as Go source in this package would spell it.
func goTypeString(t reflect.Type) string {
	if t == nil {
		return ""
	}
	pkg := reflect.TypeFor[Client]().PkgPath()
	s := strings.ReplaceAll(t.String(), pkg+".", "")
	return strings.ReplaceAll(s, "django.", "")
}
