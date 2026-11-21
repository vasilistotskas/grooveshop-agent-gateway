//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shipToStreet is homeDelivery to the street line and address line 2
// given.
func shipToStreet(street, line2 string) map[string]any {
	f := homeDelivery()
	method := f["methods"].([]any)[0].(map[string]any)
	destination := method["destinations"].([]any)[0].(map[string]any)
	destination["street_address"] = street
	if line2 != "" {
		destination["extended_address"] = line2
	}
	return f
}

// UCP: the destination's street_address is the one street line and its
// extended_address the order's address line 2, both sent as written.
// A line Django cannot split places nothing: the complete answers the
// checkout incomplete with the error at street_address, a read says the
// same, and once the platform writes the line with its number the next
// complete places the order.
func TestUCPSendsOneStreetLineAndNamesARefusedOne(t *testing.T) {
	stack := startUCPGateway(t)
	platform := httptest.NewServer(newWebhookSink().handler())
	t.Cleanup(platform.Close)
	session := connectMCP(t, stack.gw.URL)
	// One platform, one profile, named by every call, as a platform's
	// calls name theirs.
	profile := servePlatformProfile(t, platform.URL+"/ucp/orders", nil)
	meta := func(key string) map[string]any {
		m := map[string]any{"ucp-agent": map[string]any{"profile": profile}}
		if key != "" {
			m["idempotency-key"] = key
		}
		return m
	}
	buyer := map[string]any{
		"first_name": "Μαρία", "last_name": "Παπαδοπούλου",
		"email": "maria@example.test", "phone_number": "+306912345678",
	}
	lines := []any{map[string]any{
		"item": map[string]any{"id": "1"}, "quantity": 2,
	}}

	res := callTool(t, session, "create_checkout", map[string]any{
		"meta": meta(""), "cart_id": stack.faults.carts.mint(),
		"checkout": map[string]any{
			"buyer":       buyer,
			"fulfillment": shipToStreet("Πανεπιστημίου", "3ος όροφος"),
			"payment":     codPayment(),
		},
	})
	require.False(t, res.IsError, "%v", res.Content)
	created := structured(t, res)
	require.Equal(t, "ready_for_complete", created["status"],
		"a line without a number is the store's to refuse, not a guess")
	id := created["id"].(string)
	placed := stack.faults.ordersPlaced.Load()

	res = callTool(t, session, "complete_checkout", map[string]any{
		"meta": meta(uuid.NewString()), "id": id,
		"checkout": map[string]any{"payment": map[string]any{}},
	})
	require.False(t, res.IsError, "%v", res.Content)
	answer := structured(t, res)
	assert.Equal(t, "incomplete", answer["status"])
	assert.Equal(t, placed, stack.faults.ordersPlaced.Load())
	streetError := func(out map[string]any) map[string]any {
		messages, _ := out["messages"].([]any)
		for _, m := range messages {
			msg := m.(map[string]any)
			if msg["path"] ==
				"$.fulfillment.methods[0].destinations[0].street_address" {
				return msg
			}
		}
		return nil
	}
	msg := streetError(answer)
	require.NotNil(t, msg, "%v", answer["messages"])
	assert.Equal(t, "street_number_missing", msg["code"])
	assert.Equal(t, "recoverable", msg["severity"])

	res = callTool(t, session, "get_checkout", map[string]any{
		"meta": meta(""), "id": id,
	})
	require.False(t, res.IsError)
	read := structured(t, res)
	assert.Equal(t, "incomplete", read["status"])
	assert.NotNil(t, streetError(read), "the read says it too")

	res = callTool(t, session, "update_checkout", map[string]any{
		"meta": meta(""), "id": id,
		"checkout": map[string]any{
			"line_items": lines, "buyer": buyer,
			"fulfillment": shipToStreet("Πανεπιστημίου 12", "3ος όροφος"),
		},
	})
	require.False(t, res.IsError, "%v", res.Content)
	updated := structured(t, res)
	assert.Equal(t, "ready_for_complete", updated["status"])
	assert.Nil(t, streetError(updated))

	res = callTool(t, session, "complete_checkout", map[string]any{
		"meta": meta(uuid.NewString()), "id": id,
		"checkout": map[string]any{"payment": map[string]any{}},
	})
	require.False(t, res.IsError, "%v", res.Content)
	assert.Equal(t, "completed", structured(t, res)["status"])
	order := stack.faults.lastOrder.Load()
	require.NotNil(t, order)
	assert.Equal(t, "Πανεπιστημίου 12", (*order)["street"])
	assert.Equal(t, "3ος όροφος", (*order)["addressLine2"])
}

// ACP: line_one is the one street line and line_two the address line 2.
// A line Django cannot split is refused 400 at line_one and places
// nothing; the session reads not ready, with the error at line_one,
// until an update writes the line with its number.
func TestACPSendsOneStreetLineAndNamesARefusedOne(t *testing.T) {
	stack := startUCPGateway(t)
	base := stack.gw.URL + "/acp/checkout_sessions"
	address := func(lineOne string) map[string]any {
		return map[string]any{"fulfillment_details": map[string]any{
			"name": "Μαρία Παπαδοπούλου",
			"address": map[string]any{
				"name": "Μαρία Παπαδοπούλου", "line_one": lineOne,
				"line_two": "3ος όροφος", "city": "Αθήνα", "state": "GR-14",
				"country": "GR", "postal_code": "10431",
			},
		}}
	}
	id := acpReady(t, base)
	resp, _ := acpCall(t, http.MethodPost, base+"/"+id,
		address("Πανεπιστημίου"), uuid.NewString())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	placed := stack.faults.ordersPlaced.Load()

	complete := func() (*http.Response, map[string]any) {
		return acpCall(t, http.MethodPost, base+"/"+id+"/complete",
			map[string]any{"payment_data": map[string]any{}},
			uuid.NewString())
	}
	resp, refused := complete()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "%v", refused)
	assert.Equal(t, "invalid", refused["code"])
	assert.Equal(t, "$.fulfillment_details.address.line_one", refused["param"])
	assert.Equal(t, placed, stack.faults.ordersPlaced.Load())

	resp, read := acpCall(t, http.MethodGet, base+"/"+id, nil, "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "not_ready_for_payment", read["status"])
	assert.Contains(t, fmt.Sprint(read["messages"]),
		"$.fulfillment_details.address.line_one")

	resp, updated := acpCall(t, http.MethodPost, base+"/"+id,
		address("Πανεπιστημίου 12"), uuid.NewString())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ready_for_payment", updated["status"])

	resp, done := complete()
	require.Equal(t, http.StatusOK, resp.StatusCode, "%v", done)
	order := stack.faults.lastOrder.Load()
	require.NotNil(t, order)
	assert.Equal(t, "Πανεπιστημίου 12", (*order)["street"])
	assert.Equal(t, "3ος όροφος", (*order)["addressLine2"])
}
