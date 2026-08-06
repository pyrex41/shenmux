// Command mock-agent is the scripted harness entrypoint (CONTRACTS.md §4). It
// is launched inside a shenmux-run PTY as a stand-in for a real coding-agent
// CLI, driven entirely by the environment. See package mockagent for behavior.
//
// Its narration goes to stdout, which shenmux owns as the durable PTY session
// (attachable, and persisted to the history archive). Because that content is
// NOT the launching process's stdout, the operator also points MOCK_TRANSCRIPT
// at a plain file so headless tooling (`muxwork logs`) can read the transcript
// without decoding the shenmux archive. Both receive the same bytes.
package main

import (
	"io"
	"os"

	"github.com/pyrex41/shenmux/orchestrator/mockagent"
)

func main() {
	out := io.Writer(os.Stdout)
	if p := os.Getenv("MOCK_TRANSCRIPT"); p != "" {
		if f, err := os.Create(p); err == nil {
			defer f.Close()
			out = io.MultiWriter(os.Stdout, f)
		}
	}
	os.Exit(mockagent.Run(mockagent.LoadConfig(), out))
}
