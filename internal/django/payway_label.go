package django

import (
	"fmt"
	"strings"
)

// payWayLabels maps language → PayWayEnum key → shopper-facing name.
// Django stores no translated name for a pay way, only the key, so the
// copy lives with each surface; these strings match the storefront's
// payment_methods table (i18n/locales/*.json in the Nuxt repo) so an
// agent and the checkout page call a method the same thing. New tenant
// languages add one entry here; a new upstream key adds one line per
// language.
var payWayLabels = map[string]map[string]string{
	"el": {
		"CREDIT_CARD":           "Πληρωμή με Κάρτα",
		"PAY_ON_DELIVERY":       "Αντικαταβολή",
		"BOX_NOW_PAY_ON_THE_GO": "BOX NOW PAY ON THE GO!",
		"PAY_ON_STORE":          "Πληρωμή στο Κατάστημα",
		"PAY_PAL":               "PayPal",
		"STRIPE":                "Stripe",
		"VIVA_WALLET":           "Viva Wallet",
		"BANK_TRANSFER":         "Τραπεζική Κατάθεση",
		"APPLE_PAY":             "Apple Pay",
		"GOOGLE_PAY":            "Google Pay",
	},
	"en": {
		"CREDIT_CARD":           "Card Payment",
		"PAY_ON_DELIVERY":       "Cash on Delivery",
		"BOX_NOW_PAY_ON_THE_GO": "BOX NOW PAY ON THE GO!",
		"PAY_ON_STORE":          "Pay in Store",
		"PAY_PAL":               "PayPal",
		"STRIPE":                "Stripe",
		"VIVA_WALLET":           "Viva Wallet",
		"BANK_TRANSFER":         "Bank Transfer",
		"APPLE_PAY":             "Apple Pay",
		"GOOGLE_PAY":            "Google Pay",
	},
}

// Label returns the pay way's shopper-facing name in a tenant locale.
// Locales normalize by lowercasing and stripping the region ("el-GR" and
// "el_GR" → "el"); languages without a table fall back to English. A key
// this gateway does not know is an error, not a raw enum value shown to
// a shopper: it means the upstream enum grew and this table must too.
func (p PayWay) Label(locale string) (string, error) {
	lang := strings.ToLower(locale)
	if i := strings.IndexAny(lang, "-_"); i >= 0 {
		lang = lang[:i]
	}
	labels, ok := payWayLabels[lang]
	if !ok {
		labels = payWayLabels["en"]
	}
	if label, ok := labels[p.Key]; ok {
		return label, nil
	}
	return "", fmt.Errorf("pay way %d: no label for key %q", p.ID, p.Key)
}
