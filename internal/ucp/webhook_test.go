package ucp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A delivery's target is the party its endpoint belongs to: hosts under
// one registrable domain share a target, whatever their port, scheme or
// path, so subdomains cost nothing extra a share of the workers; a
// hosting domain on the public suffix list's private section is one
// target per customer; an address is itself, an IPv6 one its /64.
func TestDeliveryTarget(t *testing.T) {
	for raw, want := range map[string]string{
		"https://hooks.platform.example/ucp/orders":      "platform.example",
		"https://A.B.Platform.Example./x?token=1":        "platform.example",
		"https://platform.example:8443/orders":           "platform.example",
		"http://platform.example/orders":                 "platform.example",
		"https://orders.shop.co.uk/hook":                 "shop.co.uk",
		"https://one.github.io/hook":                     "one.github.io",
		"https://two.github.io/hook":                     "two.github.io",
		"https://203.0.113.9:8443/hook":                  "203.0.113.9",
		"https://[::ffff:203.0.113.9]/hook":              "203.0.113.9",
		"https://[2001:db8:1:2:3:4:5:6]/hook":            "2001:db8:1:2::/64",
		"https://[2001:db8:1:2:ffff::1]/hook":            "2001:db8:1:2::/64",
		"http://localhost:8080/hook":                     "localhost",
		"http://127.0.0.1:8080/hook":                     "127.0.0.1",
		"https://user@platform.example/hook":             "platform.example",
		"https://platform.example/../../other.example/x": "platform.example",
	} {
		assert.Equal(t, want, deliveryTarget(raw), raw)
	}
	assert.NotEqual(t, deliveryTarget("https://a.one.example/x"),
		deliveryTarget("https://a.two.example/x"))
}

// A slot per target is taken up to the cap and given back on release;
// targets do not share slots.
func TestTargetSlots(t *testing.T) {
	slots := newTargetSlots(2)
	assert.True(t, slots.acquire("a"))
	assert.True(t, slots.acquire("a"))
	assert.False(t, slots.acquire("a"), "a target holds at most its cap")
	assert.True(t, slots.acquire("b"), "another target is not held up")
	slots.release("a")
	assert.True(t, slots.acquire("a"))
	slots.release("a")
	slots.release("a")
	slots.release("b")
	assert.Empty(t, slots.held, "a target with nothing in flight is forgotten")
}
