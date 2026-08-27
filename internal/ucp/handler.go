package ucp

import "github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"

// The platform authors its own UCP payment handler. Neither published
// alternative is usable: Stripe's com.stripe.payments is in private
// preview, and its documented schema origin (ucp.stripe.com) fails the
// namespace authority binding UCP added in 2026-08-25, so a conforming
// platform rejects the entity outright. Viva, the primary PSP, publishes
// no UCP handler at all.
const (
	// HandlerName is the handler's reverse-domain name. Its schema is
	// served from payments.grooveshop.space, whose reversed labels equal
	// this name — the exact-match case of the authority binding. The
	// namespace is platform-owned rather than derived per tenant: the
	// binding check ignores the Public Suffix List, so a namespace
	// derived from an arbitrary tenant domain would let co-tenants under
	// a shared suffix satisfy the same prefix.
	HandlerName = "space.grooveshop.payments"

	// HandlerID identifies this handler instance. Platforms echo it as
	// `handler_id` on every instrument they submit.
	HandlerID = "grooveshop_payments"

	// HandlerVersion versions the handler contract, NOT the protocol.
	// It is deliberately independent of Version: the handler only
	// re-versions when its own shapes change, so a UCP release that
	// leaves them alone must not move these URLs.
	HandlerVersion = "2026-08-25"

	handlerBase = "https://payments.grooveshop.space/" + HandlerVersion

	// InstrumentCashOnDelivery is the instrument type for settlement on
	// delivery. It carries no credential — the schema forbids one — so
	// no participant enters PCI scope.
	InstrumentCashOnDelivery = "cash_on_delivery"
)

// AvailableInstrument advertises one instrument type a handler accepts.
type AvailableInstrument struct {
	Type string `json:"type"`
	// Constraints is a Constraint Expression over the instrument
	// schema's constraint_target — values the business derives from its
	// own configuration, never carried on the wire.
	Constraints map[string]any `json:"constraints,omitempty"`
}

// instrumentTypeFor maps a merchant pay-way provider code onto the
// instrument type that models it. Only codes with a modelled instrument
// are advertised: an unknown code has no schema for a platform to
// compose, and advertising it would offer an instrument no agent can
// construct.
func instrumentTypeFor(providerCode string) (string, bool) {
	switch providerCode {
	case "cash_on_delivery":
		return InstrumentCashOnDelivery, true
	default:
		return "", false
	}
}

// paymentHandlers advertises what an agent can settle for this tenant
// without handing the buyer to a browser. The registry is required even
// when empty, and empty is the honest answer for a store whose only
// active methods are online: those need the buyer to authenticate at the
// PSP, which UCP models as an escalation (requires_escalation plus
// continue_url), not as a payment handler.
//
// Instrument order follows the merchant's own pay-way ordering, which
// UCP reads as the business's preferred presentation.
// environmentFor maps the deployment's ENV onto the two values the
// handler config schema admits. Only the production deployment reads as
// "production": every other environment must read as a sandbox so a
// platform keeps its test traffic out of live order flow.
func environmentFor(env string) string {
	if env == "production" {
		return "production"
	}
	return "sandbox"
}

func paymentHandlers(
	t *tenant.Tenant, env string,
) map[string][]PaymentHandler {
	instruments := make([]AvailableInstrument, 0,
		len(t.AgentPaymentInstruments))
	seen := make(map[string]struct{}, len(t.AgentPaymentInstruments))
	for _, code := range t.AgentPaymentInstruments {
		kind, ok := instrumentTypeFor(code)
		if !ok {
			continue
		}
		if _, dup := seen[kind]; dup {
			continue
		}
		seen[kind] = struct{}{}
		instruments = append(instruments, AvailableInstrument{
			Type: kind,
			Constraints: map[string]any{
				"properties": map[string]any{
					"provider_code": map[string]any{"const": code},
				},
			},
		})
	}
	if len(instruments) == 0 {
		return map[string][]PaymentHandler{}
	}
	return map[string][]PaymentHandler{
		HandlerName: {{
			ID:                   HandlerID,
			Version:              HandlerVersion,
			Spec:                 handlerBase + "/",
			Schema:               handlerBase + "/schema.json",
			AvailableInstruments: instruments,
			Config: map[string]any{
				"environment": environmentFor(env),
			},
		}},
	}
}

// responsePaymentHandlers renders the AUTHORITATIVE declaration for one
// checkout. Platforms MUST treat it as final, including where it narrows
// what the profile advertised.
//
// It omits `spec` and `schema`: a platform that reached a checkout has
// already composed the handler from discovery, and the response's job is
// resolved state, not document discovery. `settlement` is added so a
// platform can tell the buyer when money actually moves.
func responsePaymentHandlers(
	t *tenant.Tenant, env string,
) map[string][]PaymentHandler {
	handlers := paymentHandlers(t, env)
	for name, entries := range handlers {
		for i := range entries {
			entries[i].Spec = ""
			entries[i].Schema = ""
			entries[i].Config = map[string]any{
				"environment": environmentFor(env),
				"settlement":  "on_delivery",
			}
		}
		handlers[name] = entries
	}
	return handlers
}
