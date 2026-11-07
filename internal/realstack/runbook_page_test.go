package realstack

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	runbookAnyBlock = regexp.MustCompile("(?ms)^```bash[^\n]*\n(.*?)^```$")
	shellVarUse     = regexp.MustCompile(`\$\{?([A-Z_][A-Z0-9_]*)`)
	shellVarSet     = regexp.MustCompile(`(?m)(?:^|[\s;])([A-Z_][A-Z0-9_]*)=`)
)

// readRunbook returns the page as a reader sees it, whatever the checkout's line
// endings.
func readRunbook(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "agent-protocols-runbook.md"))
	require.NoError(t, err)
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// Only some blocks can run against the test stack, but a person copies all of
// them, so every one must at least be valid shell.
func TestRunbookBlocksParse(t *testing.T) {
	blocks := runbookAnyBlock.FindAllStringSubmatch(readRunbook(t), -1)
	require.NotEmpty(t, blocks)

	for _, block := range blocks {
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(block[1])
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		assert.NoError(t, cmd.Run(), "%s\n%s", stderr.String(), block[1])
	}
}

// A reader may start at any flow in a fresh shell, so a block may use only a
// variable it has already set. The helpers read GW and STORE themselves.
func TestRunbookBlocksDefineTheirVariables(t *testing.T) {
	for _, block := range runbookAnyBlock.FindAllStringSubmatch(readRunbook(t), -1) {
		firstSet := map[string]int{}
		for _, m := range shellVarSet.FindAllStringSubmatchIndex(block[1], -1) {
			name := block[1][m[2]:m[3]]
			if _, seen := firstSet[name]; !seen {
				firstSet[name] = m[2]
			}
		}
		for _, m := range shellVarUse.FindAllStringSubmatchIndex(block[1], -1) {
			name := block[1][m[2]:m[3]]
			at, set := firstSet[name]
			assert.True(t, set && at < m[0],
				"%s is used before it is set in:\n%s", name, block[1])
		}
	}
}
