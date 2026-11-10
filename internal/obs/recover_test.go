package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The panic and its stack go to the log; the error names the operation
// only, with neither the panic value nor the stack.
func TestRecoverLogsThePanicAndReturnsAShortError(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	err := func() (err error) {
		defer Recover(context.Background(), log, "demo: op", &err)
		panic("boom")
	}()
	require.EqualError(t, err, "demo: op: panicked")

	var line struct {
		Level, Msg, Op, Panic, Stack string
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	assert.Equal(t, "ERROR", line.Level)
	assert.Equal(t, "panic recovered", line.Msg)
	assert.Equal(t, "demo: op", line.Op)
	assert.Equal(t, "boom", line.Panic)
	assert.Contains(t, line.Stack, "runtime/debug.Stack")
}

func TestRecoverLeavesANormalReturnAlone(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	err := func() (err error) {
		defer Recover(context.Background(), log, "demo: op", &err)
		return nil
	}()
	require.NoError(t, err)
	assert.Zero(t, buf.Len())
}
