package feeds

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
)

// validateACP validates a document against a $defs entry of the vendored
// ACP feed schema bundle.
func validateACP(t *testing.T, def string, doc any) error {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "schemas", "acp",
		"2026-04-17", "schema.feed.json")
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	raw, err := jsonschema.UnmarshalJSON(f)
	require.NoError(t, err)

	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource("acp-feed.json", raw))
	schema, err := c.Compile("acp-feed.json#/$defs/" + def)
	require.NoError(t, err)
	return schema.Validate(doc)
}
