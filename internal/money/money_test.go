package money

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMinorUnits(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
	}{
		{"empty is zero", "", 0},
		{"zero", "0", 0},
		{"two decimals", "464.68", 46468},
		{"whole number pads two zeros", "464", 46400},
		{"one decimal pads a zero", "464.6", 46460},
		{"truncates beyond two decimals", "464.689", 46468},
		{"truncation does not round up", "0.999", 99},
		{"leading dot means zero whole", ".5", 50},
		{"explicit zero cents", "0.00", 0},
		{"negative", "-5.50", -550},
		{"negative one cent", "-0.01", -1},
		{"negative truncates then negates", "-1.239", -123},
		{"large value stays exact", "1000000.00", 100000000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MinorUnits(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMinorUnitsRejectsGarbage(t *testing.T) {
	for _, in := range []string{"abc", "1.2x", "4a.68", "1,50"} {
		t.Run(in, func(t *testing.T) {
			_, err := MinorUnits(in)
			assert.Error(t, err)
		})
	}
}
