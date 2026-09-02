// Package text holds the rune-aware string helpers the protocol surfaces
// share. Everything the gateway renders is Greek-heavy, so a byte-index
// cut splits a two-byte rune roughly half the time and ships U+FFFD.
package text

import "unicode/utf8"

// Ellipsis marks a cut string.
const Ellipsis = "…"

// Runes cuts s to at most max runes without an ellipsis — for fields with
// a strict character limit (feed titles, log dumps).
func Runes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

// Ellipsize cuts s to max runes and appends Ellipsis when it did cut.
func Ellipsize(s string, max int) string {
	cut := Runes(s, max)
	if len(cut) == len(s) {
		return s
	}
	return cut + Ellipsis
}
