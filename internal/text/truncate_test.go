package text

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestRunes(t *testing.T) {
	assert.Equal(t, "", Runes("abc", 0))
	assert.Equal(t, "abc", Runes("abc", 3))
	assert.Equal(t, "abc", Runes("abc", 10))
	assert.Equal(t, "ab", Runes("abc", 2))
	// A cut never lands inside a multi-byte rune.
	greek := "Φορτιστής Κινητού"
	cut := Runes(greek, 5)
	assert.Equal(t, "Φορτι", cut)
	assert.True(t, utf8.ValidString(cut))
}

func TestEllipsize(t *testing.T) {
	assert.Equal(t, "abc", Ellipsize("abc", 3))
	assert.Equal(t, "ab…", Ellipsize("abc", 2))
	assert.Equal(t, "Φορτι…", Ellipsize("Φορτιστής", 5))
	assert.True(t, utf8.ValidString(Ellipsize("Φορτιστής", 5)))
}
