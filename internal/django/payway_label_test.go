package django

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPayWayLabel(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		locale string
		want   string
	}{
		{"greek", "PAY_ON_DELIVERY", "el", "Αντικαταβολή"},
		{"greek with region", "BANK_TRANSFER", "el-GR", "Τραπεζική Κατάθεση"},
		{"english", "PAY_ON_DELIVERY", "en", "Cash on Delivery"},
		{"untabled language falls back to english", "CREDIT_CARD", "de",
			"Card Payment"},
		{"carrier trademark is verbatim", "BOX_NOW_PAY_ON_THE_GO", "el",
			"BOX NOW PAY ON THE GO!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PayWay{Key: tc.key}.Label(tc.locale)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Every language table covers the same keys, so no tenant locale can
// hit the error branch for a key another locale knows.
func TestPayWayLabelTablesAgree(t *testing.T) {
	for lang, labels := range payWayLabels {
		assert.Len(t, labels, len(payWayLabels["en"]), lang)
		for key := range payWayLabels["en"] {
			assert.NotEmpty(t, labels[key], "%s %s", lang, key)
		}
	}
}

// An unknown or absent key is a contract drift, never a raw enum value
// or an empty label shown to a shopper.
func TestPayWayLabelRefusesUnknownKey(t *testing.T) {
	for _, key := range []string{"", "CRYPTO"} {
		_, err := PayWay{ID: 7, Key: key}.Label("el")
		assert.Error(t, err, key)
	}
}
