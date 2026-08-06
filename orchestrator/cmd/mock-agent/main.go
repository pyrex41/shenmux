// Command mock-agent is the scripted harness entrypoint (CONTRACTS.md §4). It
// is launched inside a shenmux-run PTY as a stand-in for a real coding-agent
// CLI, driven entirely by the environment. See package mockagent for behavior.
package main

import (
	"os"

	"github.com/pyrex41/shenmux/orchestrator/mockagent"
)

func main() {
	os.Exit(mockagent.Run(mockagent.LoadConfig(), os.Stdout))
}
