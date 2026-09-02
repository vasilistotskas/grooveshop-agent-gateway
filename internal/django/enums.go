package django

import (
	"maps"
	"slices"
)

// Upstream enum values the gateway branches on. They mirror Django's
// pay_way provider codes, shipping provider codes and
// order.enum.status.PaymentStatus; a rename there is a contract change
// that lands here, in one place.
const (
	ProviderVivaWallet     = "viva_wallet"
	ProviderCashOnDelivery = "cash_on_delivery"
	ShippingProviderACS    = "acs"
	ShippingProviderBoxNow = "boxnow"
	PaymentStatusCompleted = "COMPLETED"
)

// Localized picks the tenant's locale from a parler translations map,
// falling back to the alphabetically first language present so a
// product with no translation in the store's default locale still
// renders a name — deterministically, whichever pod answers.
func Localized[T any](translations map[string]T, locale string) T {
	if v, ok := translations[locale]; ok {
		return v
	}
	for _, k := range slices.Sorted(maps.Keys(translations)) {
		return translations[k]
	}
	var zero T
	return zero
}
