package checkout

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Sessions live under a versioned key, and their locks beside them: a
// pod that still writes sessions unconditionally keeps its own, under
// the unversioned key, and never shares one with a pod that fences them.
func TestSessionKeyIsVersioned(t *testing.T) {
	assert.Equal(t, "ag:alpha:cs:v2:id", sessionKey("alpha", "id"))
	assert.Equal(t, "ag:alpha:cs:v2:id:lock", lockKey("alpha", "id"))
}

// The session that holds a cart is named in a key family of its own:
// every key under a session's belongs to that session, and the scans
// that count sessions read that family.
func TestCartRecordIsOutsideTheSessionKeys(t *testing.T) {
	const cart = "29eb4495-e018-45e7-b59c-6646302bd4ef"
	assert.Equal(t, "ag:alpha:checkoutcart:"+cart, cartKey("alpha", cart))
	assert.False(t, strings.HasPrefix(cartKey("alpha", cart), "ag:alpha:cs:"))
}

// Django reads every spelling of a UUID as one cart. The gateway names a
// cart in one, so one cart has one record; what is not a UUID names no
// cart, and no key is built from it.
func TestCanonicalCartID(t *testing.T) {
	const cart = "29eb4495-e018-45e7-b59c-6646302bd4ef"
	for _, spelling := range []string{
		cart,
		strings.ToUpper(cart),
		"{" + cart + "}",
		"urn:uuid:" + cart,
		strings.ReplaceAll(cart, "-", ""),
	} {
		got, ok := CanonicalCartID(spelling)
		assert.True(t, ok, spelling)
		assert.Equal(t, cart, got, spelling)
	}
	for _, id := range []string{
		"", "cart", cart + ":lock", cart + "*", "*", cart[:35],
	} {
		got, ok := CanonicalCartID(id)
		assert.False(t, ok, id)
		assert.Empty(t, got, id)
	}
}

// A caller's id that no session can have is not found before any key is
// built from it: the store has no Redis client here, so a lock attempt
// would panic. Only the spelling a session is minted with can name one,
// so the other spellings of a UUID are refused with everything else.
func TestAnIDNoSessionCanHaveTakesNoLock(t *testing.T) {
	const minted = "6f1c2a52-3d8e-4c55-9a0f-0c6a2b9e1d11"
	assert.True(t, ValidID(minted))
	assert.True(t, ValidID(NewSession("alpha", "a.test", "ucp", "").ID))

	st := NewStore(nil, time.Second)
	for _, id := range []string{
		"",
		"not-a-session-id",
		minted + ":order",
		strings.ToUpper(minted),
		"urn:uuid:" + minted,
		"{" + minted + "}",
		strings.ReplaceAll(minted, "-", ""),
	} {
		assert.False(t, ValidID(id), id)
		_, err := st.Lock(t.Context(), "alpha", id)
		assert.ErrorIs(t, err, ErrNotFound, id)
		_, err = st.Load(t.Context(), "alpha", id)
		assert.ErrorIs(t, err, ErrNotFound, id)
	}
}

// The lock covers what a completion does under it when Django answers
// every call just inside its timeout: three lookups, then the
// completion's budget. A claim covers that and the write that settles
// it.
func TestLockAndClaimsAreSizedFromTheUpstreamTimeout(t *testing.T) {
	const timeout = 10 * time.Second
	st := NewStore(nil, timeout)
	assert.Equal(t, (3+completionSteps)*timeout, st.lockTTL)
	assert.Equal(t, st.lockTTL+timeout, st.claimTTL)
}
