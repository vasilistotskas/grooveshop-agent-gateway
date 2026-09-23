package ucp

import (
	"embed"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// specFS is the vendored UCP schema set for Version (see spec/README.md).
// It ships in the binary because platform profiles are validated against
// the spec's own schemas at request time, not only in tests.
//
//go:embed spec/2026-08-25
var specFS embed.FS

const specRefBase = "https://ucp.dev/schemas/"

// specLoader resolves https://ucp.dev/schemas/* refs from the embedded
// copy and refuses every other URL: validation never reaches the network.
type specLoader struct{}

func (specLoader) Load(url string) (any, error) {
	if !strings.HasPrefix(url, specRefBase) {
		return nil, fmt.Errorf("ucp: schema ref outside the vendored spec: %s",
			url)
	}
	f, err := specFS.Open("spec/" + Version + "/" +
		strings.TrimPrefix(url, specRefBase))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return jsonschema.UnmarshalJSON(f)
}

// CompileSpec compiles a vendored schema, e.g.
// "profile.json#/$defs/platform_schema".
func CompileSpec(ref string) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	c.UseLoader(specLoader{})
	return c.Compile(specRefBase + ref)
}
