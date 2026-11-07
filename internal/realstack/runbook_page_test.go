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

var runbookAnyBlock = regexp.MustCompile("(?ms)^```bash[^\n]*\n(.*?)^```$")

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
