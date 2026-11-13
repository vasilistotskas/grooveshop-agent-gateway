package checkout

import (
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

// A caller's id that no session can have is not found before any key is
// built from it: the store has no Redis client here, so a lock attempt
// would panic.
func TestAnIDNoSessionCanHaveTakesNoLock(t *testing.T) {
	st := NewStore(nil, time.Second)
	_, err := st.Lock(t.Context(), "alpha", "not-a-session-id")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = st.Load(t.Context(), "alpha", "not-a-session-id")
	assert.ErrorIs(t, err, ErrNotFound)
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
