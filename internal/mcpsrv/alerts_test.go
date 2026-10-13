package mcpsrv

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAlertTargetPrice(t *testing.T) {
	valid := map[string]json.Number{
		"":      "",
		"19.9":  "19.90",
		"19.90": "19.90",
		"20":    "20.00",
		"20.":   "20.00",
	}
	for in, want := range valid {
		got, err := alertTargetPrice(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"abc", "19,90", "-5", "0", "1e3"} {
		_, err := alertTargetPrice(in)
		assert.Error(t, err, in)
	}
}
