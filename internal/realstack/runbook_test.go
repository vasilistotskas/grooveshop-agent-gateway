//go:build realstack

package realstack

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The runbook's flow, and what its output must show. A new `bash runbook` block
// without an entry here fails the test, so nothing is run without being checked.
var runbookExpects = map[string][]string{
	"M1": {"/cart/claim?uuid="},
	"U1": {"incomplete", "ready_for_complete", "completed Order", "PENDING"},
	"U4": {"canceled"},
	"A1": {"HTTP 201", "completed"},
}

var (
	runbookHeading = regexp.MustCompile(`(?m)^### ([A-Z]\d+)\.`)
	runbookBlock   = regexp.MustCompile("(?s)```bash runbook\n(.*?)```")
)

// runbookBlocks reads the page's runnable blocks, keyed by their flow.
func runbookBlocks(t *testing.T, page string) map[string]string {
	t.Helper()
	blocks := map[string]string{}
	headings := runbookHeading.FindAllStringSubmatchIndex(page, -1)
	for i, h := range headings {
		end := len(page)
		if i+1 < len(headings) {
			end = headings[i+1][0]
		}
		if m := runbookBlock.FindStringSubmatch(page[h[0]:end]); m != nil {
			blocks[page[h[2]:h[3]]] = m[1]
		}
	}
	return blocks
}

// The commands a person or a later session copies must work, so they run here
// exactly as the page prints them.
func TestRunbookCommandsRunAsWritten(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	page := readRunbook(t)
	blocks := runbookBlocks(t, page)

	// One block per flow, each under a flow's heading: any other would not run.
	require.Len(t, blocks, len(runbookBlock.FindAllString(page, -1)))
	for flow := range runbookExpects {
		require.Contains(t, blocks, flow, "the page no longer runs this flow")
	}

	for flow, block := range blocks {
		t.Run(flow, func(t *testing.T) {
			want, known := runbookExpects[flow]
			require.True(t, known, "flow %s has no expected output", flow)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c",
				"set -eo pipefail\n. scripts/agent-lib.sh\n"+block)
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"GW="+running.gatewayURL,
				"STORE="+storeHost,
				"UCP_PROFILE="+running.profileURL,
				"ACP_TOKEN="+running.seeded.ACPToken,
			)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
			for _, text := range want {
				assert.Contains(t, string(out), text)
			}
		})
	}
}

// The smoke check runs against production, so here it is only parsed.
func TestSmokeScriptParses(t *testing.T) {
	cmd := exec.Command("bash", "-n", filepath.Join("..", "..", "scripts", "agent-smoke.sh"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}
