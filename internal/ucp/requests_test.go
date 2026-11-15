package ucp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The request schemas are generated from the vendored spec by a tool CI
// does not have, so what is checked is what they were generated from: the
// digest scripts/ucp-request-schemas.sh wrote beside them must be the
// digest of the spec tree and the composition embedded now. A re-vendor
// without a regeneration fails here.
func TestRequestSchemasAreCurrent(t *testing.T) {
	var paths []string
	root := "spec/" + Version
	require.NoError(t, fs.WalkDir(specFS, root,
		func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				paths = append(paths, strings.TrimPrefix(path, root+"/"))
			}
			return err
		}))
	sort.Strings(paths)

	sum := func(name string) string {
		raw, err := fs.ReadFile(specFS, name)
		if err != nil {
			raw, err = fs.ReadFile(requestFS, name)
		}
		require.NoError(t, err)
		h := sha256.Sum256(raw)
		return hex.EncodeToString(h[:])
	}
	var lines strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&lines, "%s %s/%s\n", sum(root+"/"+p), Version, p)
	}
	fmt.Fprintf(&lines, "%s requests/compose.json\n",
		sum("spec/requests/compose.json"))
	want := sha256.Sum256([]byte(lines.String()))

	got, err := fs.ReadFile(requestFS, "spec/requests/digest")
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(want[:]),
		strings.TrimSpace(string(got)),
		"regenerate with scripts/ucp-request-schemas.sh")
}

// The composition names this version and exactly the capabilities the
// business advertises that add request members.
func TestRequestSchemasComposeWhatTheBusinessAdvertises(t *testing.T) {
	raw, err := fs.ReadFile(requestFS, "spec/requests/compose.json")
	require.NoError(t, err)
	var stub PlatformProfile
	require.NoError(t, json.Unmarshal(raw, &stub))
	assert.Equal(t, Version, stub.UCP.Version)

	names := make([]string, 0, len(stub.UCP.Capabilities))
	for name, caps := range stub.UCP.Capabilities {
		names = append(names, name)
		require.Len(t, caps, 1)
		assert.Equal(t, Version, caps[0].Version)
	}
	assert.ElementsMatch(t,
		[]string{CapabilityCheckout, CapabilityDiscount}, names)

	for _, op := range []Operation{OpCreate, OpUpdate, OpComplete} {
		var schema map[string]any
		require.NoError(t, json.Unmarshal(CheckoutRequestSchema(op), &schema),
			op)
	}
}
