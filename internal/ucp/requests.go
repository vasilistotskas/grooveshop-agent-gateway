package ucp

import (
	"embed"
	"encoding/json"
)

// requestFS holds the checkout request schemas the spec's own tool
// resolved from the vendored spec, with what it was given
// (spec/README.md, scripts/ucp-request-schemas.sh).
//
//go:embed spec/requests
var requestFS embed.FS

// Operation is a checkout operation with a request body of its own.
type Operation string

const (
	OpCreate   Operation = "create"
	OpUpdate   Operation = "update"
	OpComplete Operation = "complete"
)

// CheckoutRequestSchema is the spec's schema for the checkout a request of
// op carries, composed with every extension the business advertises that
// adds request members (spec/requests/compose.json). It is fully bundled
// and open: it accepts members it does not describe.
func CheckoutRequestSchema(op Operation) json.RawMessage {
	raw, err := requestFS.ReadFile("spec/requests/checkout_" + string(op) +
		".json")
	if err != nil {
		panic("ucp: no request schema for " + string(op) + ": " + err.Error())
	}
	return raw
}
