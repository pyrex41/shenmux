// Command muxwork is the orchestration control plane for the demo: it manages
// worker OS processes (in place of Kubernetes pods) via the operator package.
package main

import (
	"os"

	"github.com/pyrex41/shenmux/orchestrator/muxwork"
)

func main() {
	os.Exit(muxwork.Run(os.Args[1:], os.Stdout, os.Stderr))
}
