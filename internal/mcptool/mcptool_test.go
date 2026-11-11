package mcptool

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The tool result is JSON an agent can read field by field, Greek
// unescaped.
func TestToolErrorRendersAsJSON(t *testing.T) {
	err := &ToolError{Code: "insufficient_stock", Detail: "Μόνο 2 <διαθέσιμα>",
		Hint: "explain"}

	assert.JSONEq(t, `{"code":"insufficient_stock",`+
		`"detail":"Μόνο 2 <διαθέσιμα>","hint":"explain"}`, err.Error())
	assert.Contains(t, err.Error(), "Μόνο 2 <διαθέσιμα>")
}
