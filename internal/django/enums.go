package django

import (
	"maps"
	"slices"
)

// Upstream enum values the gateway branches on. They mirror Django's
// pay_way provider codes, shipping provider codes and
// order.enum.status (OrderStatus, PaymentStatus); a rename there is a
// contract change that lands here, in one place.
const (
	OrderStatusCanceled     = "CANCELED"
	PaymentStatusFailed     = "FAILED"
	PaymentStatusProcessing = "PROCESSING"
	PaymentStatusRefunded   = "REFUNDED"
	PaymentStatusPartRefund = "PARTIALLY_REFUNDED"
	ProviderVivaWallet      = "viva_wallet"
	ProviderCashOnDelivery  = "cash_on_delivery"
	ShippingProviderACS     = "acs"
	ShippingProviderBoxNow  = "boxnow"
	ShippingKindPickupPoint = "pickup_point"
	PaymentStatusCompleted  = "COMPLETED"
)

// ChargedPaymentStatuses are the payment states in which the buyer's
// money was taken, kept or refunded since (Django's
// CHARGED_PAYMENT_STATUSES).
var ChargedPaymentStatuses = []string{
	PaymentStatusCompleted, PaymentStatusRefunded, PaymentStatusPartRefund,
}

// The codes of Django's 400 refusing a payment link for an order
// (order/views/order.py, _payment_start_refusal): its payment window has
// run out, or it is paid or no longer open for payment. Both are final
// for that order.
const (
	CodePaymentWindowExpired = "payment_window_expired"
	CodeOrderNotPayable      = "order_not_payable"
)

// CodeCartNotFound is the `code` of Django's 404 for a cart it does not
// have (cart/views/cart.py). A 404 without it is another refusal, such as
// a feature gate's bare NotFound.
const CodeCartNotFound = "cart_not_found"

// CodeCartHasUnpaidOrder is the `code` of Django's 400 refusing an agent's
// order from a cart whose earlier order still awaits its online payment
// (order/views/order.py, CartHasUnpaidOrderError). CreateOrder reports it
// as ErrCartHasUnpaidOrder.
const CodeCartHasUnpaidOrder = "cart_has_unpaid_order"

// CodeTotalChanged is the `code` of Django's 409 refusing an order whose
// expectedTotal is not the amount due it computes now
// (order/enum/create_error.py). CreateOrder reports it as
// ErrTotalChanged.
const CodeTotalChanged = "total_changed"

// CodeStreetNumberMissing and CodeStreetNumberIsPostcode are the codes of
// Django's refusal of an order's street line, on `street`: no house
// number can be split from it, or the number split is a postcode
// (core/validators/address.py). CreateOrder reports them as a
// *StreetRefusedError.
const (
	CodeStreetNumberMissing    = "street_number_missing"
	CodeStreetNumberIsPostcode = "street_number_is_postcode"
)

// CodeCartChanged is the `code` of Django's 409 refusing an order whose
// expectedCartDigest is not the digest of the cart it would be made of
// now (order/enum/create_error.py). CreateOrder reports it as
// ErrCartChanged.
const CodeCartChanged = "cart_changed"

// CodePayWayUnavailable is the `code` of Django's 400 refusing a pay way
// the store does not offer for the chosen delivery (cart/views/cart.py).
const CodePayWayUnavailable = "pay_way_unavailable"

// CodePayWayNotFound is the `code` of Django's 404 for a pay way it does
// not have (cart/views/cart.py).
const CodePayWayNotFound = "pay_way_not_found"

// CodeCartEmpty is the `code` of Django's 400 refusing to price or order
// a cart with no lines.
const CodeCartEmpty = "cart_empty"

// The codes of Django's 400 refusing a delivery that cannot be priced for
// a cart (shipping/enum/refusal.py, ShippingRefusal): the store does not
// ship that way to the country, or the cart does not fit it.
const (
	CodeShippingUnavailable    = "shipping_unavailable"
	CodeShippingParcelTooLarge = "shipping_parcel_too_large"
	CodeShippingWeightExceeded = "shipping_weight_exceeded"
)

// AgentProtocolMCP is the attribution agentProtocol (Django's
// AGENT_PROTOCOLS) of a cart an agent fills through the /mcp commerce
// tools. A UCP or ACP checkout sends its own protocol
// (checkout.ProtocolUCP, checkout.ProtocolACP).
const AgentProtocolMCP = "mcp"

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
