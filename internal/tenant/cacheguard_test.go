package tenant

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// goldenKeyVersion is the redisKeyVersion that goldenShape belongs to.
const goldenKeyVersion = "v5"

// goldenShape is every field of the cached django.TenantConfig, nested
// types included: "path type json-tag", sorted. It is the shape the
// entries under goldenKeyVersion hold.
//
// Changing the shape means: bump redisKeyVersion (resolver.go), then update
// goldenKeyVersion and these lines. A pod running the new code must never
// read an entry an older pod wrote without the new field, or the field
// reads as its zero value until the entry expires.
//
// The test prompts the bump; it cannot prove it. Updating these lines
// alone passes, and the golden's diff in review is what shows a shape
// change without one. Not seen, and fail-safe: a named type whose
// underlying type changes, and `,string` added to a tag.
var goldenShape = []string{
	`TenantConfig.ACPBearerToken string "acpBearerToken"`,
	`TenantConfig.AgentCommerceEnabled bool "agentCommerceEnabled"`,
	`TenantConfig.AgentHostedPaymentEnabled bool "agentHostedPaymentEnabled"`,
	`TenantConfig.AgentPaymentInstruments []string "agentPaymentInstruments"`,
	`TenantConfig.AssetsDomain string "assetsDomain"`,
	`TenantConfig.AvailableLocales []string "availableLocales"`,
	`TenantConfig.ChatAPIKey string "chatApiKey"`,
	`TenantConfig.DefaultCurrency string "defaultCurrency"`,
	`TenantConfig.DefaultLocale string "defaultLocale"`,
	`TenantConfig.FaviconURL string "faviconUrl"`,
	`TenantConfig.Features django.StoreFeatures "features"`,
	`TenantConfig.Features.Cart bool "cart"`,
	`TenantConfig.Features.Catalogue bool "catalogue"`,
	`TenantConfig.Features.Favourites bool "favourites"`,
	`TenantConfig.Features.Loyalty bool "loyalty"`,
	`TenantConfig.Features.ProductAlerts bool "productAlerts"`,
	`TenantConfig.Features.ProductReviews bool "productReviews"`,
	`TenantConfig.Features.Promotions bool "promotions"`,
	`TenantConfig.Features.StaffAIAccess bool "staffAiAccess"`,
	`TenantConfig.LogoDarkURL string "logoDarkUrl"`,
	`TenantConfig.LogoLightURL string "logoLightUrl"`,
	`TenantConfig.Name string "name"`,
	`TenantConfig.PrimaryDomain string "primaryDomain"`,
	`TenantConfig.ProductFeedsEnabled bool "productFeedsEnabled"`,
	`TenantConfig.SchemaName string "schemaName"`,
	`TenantConfig.StoreDescription string "storeDescription"`,
	`TenantConfig.StoreName string "storeName"`,
	`TenantConfig.StripePublishableKey string "stripePublishableKey"`,
}

// fieldInfo is one serialised field of the cached shape.
type fieldInfo struct {
	path  string
	field reflect.StructField
	value reflect.Value // set by undocumentedFields only
}

// serialised reports whether encoding/json writes the field: an exported
// one, or an embedded struct, exported or not, whose own exported fields
// are written as if they were the parent's.
func serialised(f reflect.StructField) bool {
	if f.Tag.Get("json") == "-" {
		return false
	}
	if f.IsExported() {
		return true
	}
	return f.Anonymous && indirect(f.Type).Kind() == reflect.Struct
}

func indirect(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// hasContractTag reports whether the field's contract tag lists tag. The
// tag is a comma-separated list, as internal/django/contract_test.go
// reads it.
func hasContractTag(f reflect.StructField, tag string) bool {
	return slices.Contains(strings.Split(f.Tag.Get("contract"), ","), tag)
}

// shapeFields lists every field json.Marshal writes for t, through nested
// structs and the element types of pointers, slices, arrays and maps. That
// is exactly what the Redis entry holds. A type that contains itself has no
// finite shape and is an error.
func shapeFields(t reflect.Type, path string) ([]fieldInfo, error) {
	return walkShape(t, path, nil)
}

func walkShape(
	t reflect.Type, path string, onPath []reflect.Type,
) ([]fieldInfo, error) {
	for {
		if slices.Contains(onPath, t) {
			return nil, fmt.Errorf("%s: type %s contains itself", path, t)
		}
		onPath = append(onPath, t)
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
			continue
		}
		break
	}
	if t.Kind() != reflect.Struct {
		return nil, nil
	}
	var out []fieldInfo
	for f := range t.Fields() {
		if !serialised(f) {
			continue
		}
		p := path + "." + f.Name
		out = append(out, fieldInfo{path: p, field: f})
		nested, err := walkShape(f.Type, p, onPath)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

func fingerprint(t reflect.Type) ([]string, error) {
	fields, err := shapeFields(t, t.Name())
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, fi := range fields {
		name, _, _ := strings.Cut(fi.field.Tag.Get("json"), ",")
		lines = append(lines, fmt.Sprintf("%s %s %q",
			fi.path, fi.field.Type, name))
	}
	slices.Sort(lines)
	return lines, nil
}

// shapeDiff names what differs between the golden and the current shape.
func shapeDiff(golden, got []string) string {
	var b strings.Builder
	for _, l := range golden {
		if !slices.Contains(got, l) {
			b.WriteString("\n  - " + l)
		}
	}
	for _, l := range got {
		if !slices.Contains(golden, l) {
			b.WriteString("\n  + " + l)
		}
	}
	return b.String()
}

// A change to the cached shape without a new key version serves the new
// field as its zero value from every entry an older pod wrote.
func TestCachedShapeMatchesKeyVersion(t *testing.T) {
	got, err := fingerprint(reflect.TypeFor[django.TenantConfig]())
	require.NoError(t, err)

	shapeSame := slices.Equal(got, goldenShape)
	versionSame := redisKeyVersion == goldenKeyVersion

	switch {
	case shapeSame && versionSame:
	case !shapeSame && versionSame:
		t.Fatalf("TenantConfig's cached shape changed: bump the version "+
			"in redisKey (redisKeyVersion, now %q), update the key "+
			"literal in internal/integration/redis_resolver_test.go, "+
			"then update this golden (goldenKeyVersion and "+
			"goldenShape):%s",
			redisKeyVersion, shapeDiff(goldenShape, got))
	case shapeSame && !versionSame:
		t.Fatalf("redisKeyVersion is %q but the golden still describes %q: "+
			"update goldenKeyVersion (the shape itself is unchanged)",
			redisKeyVersion, goldenKeyVersion)
	default:
		t.Fatalf("TenantConfig's cached shape changed and redisKeyVersion "+
			"is now %q: update goldenKeyVersion (%q) and goldenShape:%s",
			redisKeyVersion, goldenKeyVersion, shapeDiff(goldenShape, got))
	}
}

func TestRedisKeyCarriesTheVersion(t *testing.T) {
	assert.Equal(t, "ag:tenant:"+redisKeyVersion+":shop.example.test",
		redisKey("shop.example.test"))
}

// undocumentedFields lists the fields of v tagged contract:"undocumented".
func undocumentedFields(v reflect.Value, path string) []fieldInfo {
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	var out []fieldInfo
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !serialised(f) {
				continue
			}
			p := path + "." + f.Name
			if hasContractTag(f, "undocumented") {
				out = append(out, fieldInfo{path: p, field: f, value: v.Field(i)})
			}
			out = append(out, undocumentedFields(v.Field(i), p)...)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			out = append(out, undocumentedFields(v.Index(i), path)...)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			out = append(out, undocumentedFields(v.MapIndex(k), path)...)
		}
	}
	return out
}

// fill gives v a value in every position of the cached shape: containers
// get one element and every field gets a non-zero value, so a tagged field
// anywhere in the shape is reached. A string field also carries its path,
// which lets a leak be named in the serialised entry. The type must have
// passed shapeFields: a type that contains itself never ends here.
func fill(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if serialised(f) {
				fill(t, v.Field(i), path+"."+f.Name)
			}
		}
	case reflect.Pointer:
		if !v.CanSet() {
			t.Fatalf("teach fill the embedded unexported pointer %s", path)
		}
		v.Set(reflect.New(v.Type().Elem()))
		fill(t, v.Elem(), path)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(t, v.Index(0), path)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k := reflect.New(v.Type().Key()).Elem()
		fill(t, k, path+".key")
		e := reflect.New(v.Type().Elem()).Elem()
		fill(t, e, path)
		v.SetMapIndex(k, e)
	case reflect.String:
		v.SetString("value-of-" + path)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
		reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32,
		reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	default:
		t.Fatalf("teach fill the kind %s of %s", v.Kind(), path)
	}
}

func marshalString(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

// Every credential in the cached shape carries contract:"undocumented":
// the rule says a field Django leaves out of its schema is one it sends
// only to the gateway's authenticated call, and none of those is written
// to the shared Redis entry. publicConfig must clear each one, found here
// by reflection, so a credential added to TenantConfig is covered without
// editing this test. Tag a new credential the same way.
func TestPublicConfigClearsEveryUndocumentedField(t *testing.T) {
	var cfg django.TenantConfig
	_, err := shapeFields(reflect.TypeFor[django.TenantConfig](), "TenantConfig")
	require.NoError(t, err)
	fill(t, reflect.ValueOf(&cfg).Elem(), "TenantConfig")
	tagged := undocumentedFields(reflect.ValueOf(cfg), "TenantConfig")
	require.NotEmpty(t, tagged, "no field carries contract:\"undocumented\"")
	before := marshalString(t, &cfg)

	public := publicConfig(&cfg)

	// The memory tier keeps the caller's config whole.
	assert.True(t, before == marshalString(t, &cfg),
		"publicConfig changed the config it was given (a slice or map "+
			"it shares with the copy?)")

	for _, fi := range undocumentedFields(
		reflect.ValueOf(public), "TenantConfig") {
		assert.True(t, fi.value.IsZero(),
			"publicConfig leaves %s set: clear it, or the shared Redis "+
				"holds the credential in plain text", fi.path)
	}

	raw := marshalString(t, &public)
	for _, fi := range tagged {
		if fi.field.Type.Kind() != reflect.String {
			continue
		}
		needle := marshalString(t, "value-of-"+fi.path)
		assert.False(t, strings.Contains(raw, needle),
			"%s is written to the shared Redis entry", fi.path)
	}
	// What is not a credential still rides the cache.
	assert.Equal(t, "value-of-TenantConfig.SchemaName", public.SchemaName)
	assert.True(t, public.Features.Cart)
}

// withCredentials puts back every credential publicConfig clears, so a
// credential answer built from a public config and held credentials is
// whole. A credential added to publicConfig without withCredentials fails
// here.
func TestWithCredentialsRestoresWhatPublicConfigClears(t *testing.T) {
	var cfg django.TenantConfig
	fill(t, reflect.ValueOf(&cfg).Elem(), "TenantConfig")
	public := publicConfig(&cfg)

	whole := withCredentials(public, cfg)

	assert.True(t, marshalString(t, &cfg) == marshalString(t, &whole),
		"withCredentials leaves a credential publicConfig clears unset")
}

// The two walkers, on types the guards have to read right.
type (
	walkerLeaf struct {
		Key  string `json:"key" contract:"undocumented,optional"`
		Hint string `json:"hint"`
	}
	walkerHidden struct {
		Inner string `json:"inner" contract:"undocumented"`
	}
	walkerShape struct {
		walkerHidden
		Leaves  []walkerLeaf `json:"leaves"`
		Skipped string       `json:"-" contract:"undocumented"`
		private string
	}
	walkerNode struct {
		Children walkerTree `json:"children"`
	}
	walkerTree []walkerNode
)

func TestGuardWalkersReadWhatJSONWrites(t *testing.T) {
	fields, err := shapeFields(reflect.TypeFor[walkerShape](), "S")
	require.NoError(t, err)
	var paths []string
	for _, fi := range fields {
		paths = append(paths, fi.path)
	}
	assert.Equal(t, []string{
		"S.walkerHidden", "S.walkerHidden.Inner",
		"S.Leaves", "S.Leaves.Key", "S.Leaves.Hint",
	}, paths, "an embedded unexported struct is written, a json:\"-\" "+
		"and an unexported field are not")

	var s walkerShape
	s.private = "not written by encoding/json"
	fill(t, reflect.ValueOf(&s).Elem(), "S")
	var tagged []string
	for _, fi := range undocumentedFields(reflect.ValueOf(s), "S") {
		tagged = append(tagged, fi.path)
	}
	assert.Equal(t, []string{"S.walkerHidden.Inner", "S.Leaves.Key"}, tagged,
		"the tag is a comma-separated list")
}

func TestShapeFieldsRejectsATypeThatContainsItself(t *testing.T) {
	_, err := shapeFields(reflect.TypeFor[walkerNode](), "N")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contains itself")
}
