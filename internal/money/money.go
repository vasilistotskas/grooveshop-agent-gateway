// Package money converts the API's dot-decimal amounts into ISO 4217 minor
// units without float rounding. UCP and ACP both mandate integer minor
// units on the wire.
package money

import (
	"fmt"
	"strconv"
	"strings"
)

// MinorUnits converts "464.68" to 46468. Fractions beyond two places are
// truncated (upstream money is always 2dp EUR).
func MinorUnits(decimal string) (int64, error) {
	if decimal == "" {
		return 0, nil
	}
	neg := strings.HasPrefix(decimal, "-")
	if neg {
		decimal = decimal[1:]
	}
	whole, frac, _ := strings.Cut(decimal, ".")
	if whole == "" {
		whole = "0"
	}
	switch len(frac) {
	case 0:
		frac = "00"
	case 1:
		frac += "0"
	case 2:
	default:
		frac = frac[:2]
	}
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: bad amount %q: %w", decimal, err)
	}
	if neg {
		n = -n
	}
	return n, nil
}

// Format renders minor units as the dot-decimal string the feeds print
// (46468 → "464.68"), the inverse of MinorUnits.
func Format(minor int64) string {
	if minor < 0 {
		// Unsigned magnitude: negating math.MinInt64 would overflow.
		magnitude := uint64(-(minor + 1)) + 1
		return fmt.Sprintf("-%d.%02d", magnitude/100, magnitude%100)
	}
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}
