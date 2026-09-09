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

// Settlement values, mirroring Django's “pay_way.enum.settlement
// .PaySettlement“. This is how the money changes hands, and it is the
// authoritative discriminator — the “isOnlinePayment“ /
// “requiresConfirmation“ booleans this gateway used to branch on are
// deprecated mirrors upstream, scheduled for removal.
//
// That removal is exactly why they are gone from here. A bool absent
// from a JSON payload decodes to “false“, indistinguishable from a
// real “false“ (Go's encoding/json cannot tell the two apart, in v1
// or v2), so the day the columns dropped every pay way would silently
// have read "offline": online-only methods offered to agents as
// collect-on-delivery instruments, and card payments routed down the
// offline completion path. A settlement that is absent or unrecognised
// is instead neither online NOR collected-later, so every caller that
// branches on it fails closed.
const (
	SettlementOnline          = "online"
	SettlementCourierCash     = "courier_cash"
	SettlementCarrierTerminal = "carrier_terminal"
	SettlementOfflineTransfer = "offline_transfer"
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
