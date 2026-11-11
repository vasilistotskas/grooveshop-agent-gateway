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

// fieldInfo is one exported, serialised field of the cached shape.
type fieldInfo struct {
	path  string
	field reflect.StructField
	value reflect.Value // set by undocumentedFields only
}

// shapeFields lists every field that json.Marshal writes for t, through
// nested structs and the element types of pointers, slices, arrays and
// maps. That is exactly what the Redis entry holds.
func shapeFields(t reflect.Type, path string) []fieldInfo {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice ||
		t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []fieldInfo
	for f := range t.Fields() {
		if !f.IsExported() || f.Tag.Get("json") == "-" {
			continue
		}
		p := path + "." + f.Name
		out = append(out, fieldInfo{path: p, field: f})
		out = append(out, shapeFields(f.Type, p)...)
	}
	return out
}

func fingerprint(t reflect.Type) []string {
	var lines []string
	for _, fi := range shapeFields(t, t.Name()) {
		name, _, _ := strings.Cut(fi.field.Tag.Get("json"), ",")
		lines = append(lines, fmt.Sprintf("%s %s %q",
			fi.path, fi.field.Type, name))
	}
	slices.Sort(lines)
	return lines
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
	got := fingerprint(reflect.TypeFor[django.TenantConfig]())

	shapeSame := slices.Equal(got, goldenShape)
	versionSame := redisKeyVersion == goldenKeyVersion

	switch {
	case shapeSame && versionSame:
	case !shapeSame && versionSame:
		t.Fatalf("TenantConfig's cached shape changed: bump the version "+
			"in redisKey (redisKeyVersion, now %q), then update this "+
			"golden (goldenKeyVersion and goldenShape):%s",
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

// undocumentedFields lists the fields of v tagged contract:"undocumented":
// the internal-only credentials Django sends only to the gateway.
func undocumentedFields(v reflect.Value, path string) []fieldInfo {
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	var out []fieldInfo
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			p := path + "." + f.Name
			if f.Tag.Get("contract") == "undocumented" {
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
// which lets a leak be named in the serialised entry.
func fill(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.IsExported() {
				fill(t, v.Field(i), path+"."+f.Name)
			}
		}
	case reflect.Pointer:
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

// Every credential in the cached shape carries contract:"undocumented"
// (Django documents none of them: it sends them only to a caller holding
// the internal token). publicConfig must clear each one, found here by
// reflection, so a credential added to TenantConfig is covered without
// editing this test. Tag a new credential the same way.
func TestPublicConfigClearsEveryUndocumentedField(t *testing.T) {
	var cfg django.TenantConfig
	fill(t, reflect.ValueOf(&cfg).Elem(), "TenantConfig")

	tagged := undocumentedFields(reflect.ValueOf(cfg), "TenantConfig")
	require.NotEmpty(t, tagged, "no field carries contract:\"undocumented\"")

	public := publicConfig(&cfg)

	for _, fi := range undocumentedFields(
		reflect.ValueOf(public), "TenantConfig") {
		assert.True(t, fi.value.IsZero(),
			"publicConfig leaves %s set: clear it, or the shared Redis "+
				"holds the credential in plain text", fi.path)
	}

	raw, err := json.Marshal(&public)
	require.NoError(t, err)
	for _, fi := range tagged {
		assert.False(t, strings.Contains(string(raw), "value-of-"+fi.path),
			"%s is written to the shared Redis entry", fi.path)
	}
	// What is not a credential still rides the cache.
	assert.Equal(t, "value-of-TenantConfig.SchemaName", public.SchemaName)
	assert.True(t, public.Features.Cart)
}
