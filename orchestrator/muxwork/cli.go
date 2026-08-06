// Package muxwork implements the muxwork command-line interface: argument
// parsing and output formatting on top of the operator package (which does the
// actual worker process management).
package muxwork

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pyrex41/shenmux/orchestrator/operator"
)

const usage = `muxwork - local orchestration control plane (demo mode)

usage:
  muxwork --state DIR <command> [flags]

commands:
  spawn    --name NAME --harness HARNESS [--prompt P] [--from SRC | --checkpoint MANIFEST]
                                         [--mock-target URL] [--mock-steps N]
  list                                   table of workers
  status   --name NAME                   print a worker's status.json
  suspend  --name NAME                   checkpoint + stop processes, keep state
  resume   --name NAME                   restart from last checkpoint (new proxy port)
  fork     --name NEW --from SRC [--prompt P]   snapshot-fork SRC into a new worker
  logs     --name NAME [--lines N]       tail the worker session log
  watch    [--json]                      stream lifecycle events across all workers
  gc       [--ttl DUR]                   remove Completed workers older than ttl

global:
  --state DIR   state directory holding workers/ and store/ (required)
`

// Run parses args (excluding the program name) and executes a subcommand.
// out/errOut are where normal and diagnostic output go.
func Run(args []string, out, errOut io.Writer) int {
	state, sub, rest := splitArgs(args)

	if sub == "" || sub == "help" || sub == "-h" || sub == "--help" {
		fmt.Fprint(out, usage)
		if sub == "" {
			return 2
		}
		return 0
	}

	if state == "" {
		fmt.Fprintln(errOut, "error: --state DIR is required")
		fmt.Fprint(errOut, usage)
		return 2
	}

	var err error
	switch sub {
	case "spawn":
		err = cmdSpawn(state, rest, out, errOut)
	case "list":
		err = cmdList(state, rest, out)
	case "status":
		err = cmdStatus(state, rest, out)
	case "suspend":
		err = cmdSuspend(state, rest, out, errOut)
	case "resume":
		err = cmdResume(state, rest, out, errOut)
	case "fork":
		err = cmdFork(state, rest, out, errOut)
	case "logs":
		err = cmdLogs(state, rest, out)
	case "watch":
		err = cmdWatch(state, rest, out)
	case "gc":
		err = cmdGC(state, rest, out)
	default:
		fmt.Fprintf(errOut, "error: unknown command %q\n", sub)
		fmt.Fprint(errOut, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	return 0
}

// splitArgs pulls out the global --state flag and the subcommand, returning the
// remaining args for the subcommand's own flag set. --state is accepted both
// before and after the subcommand.
func splitArgs(args []string) (state, sub string, rest []string) {
	i := 0
	takeState := func(a string) (string, bool) {
		switch {
		case a == "--state" || a == "-state":
			return "", true // value is the next token
		case strings.HasPrefix(a, "--state="):
			return strings.TrimPrefix(a, "--state="), false
		case strings.HasPrefix(a, "-state="):
			return strings.TrimPrefix(a, "-state="), false
		}
		return "", false
	}
	for i < len(args) {
		a := args[i]
		if v, needNext := takeState(a); needNext || v != "" {
			if needNext {
				if i+1 < len(args) {
					state = args[i+1]
					i += 2
					continue
				}
				i++
				continue
			}
			state = v
			i++
			continue
		}
		if sub == "" && !strings.HasPrefix(a, "-") {
			sub = a
			i++
			// everything after the subcommand is its own, except a trailing --state
			for i < len(args) {
				b := args[i]
				if v, needNext := takeState(b); needNext || v != "" {
					if needNext {
						if i+1 < len(args) {
							state = args[i+1]
							i += 2
							continue
						}
						i++
						continue
					}
					state = v
					i++
					continue
				}
				rest = append(rest, b)
				i++
			}
			return
		}
		// A flag before the subcommand that isn't --state: pass it through.
		if sub == "" {
			rest = append(rest, a)
		}
		i++
	}
	return
}

func cmdSpawn(state string, args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	fs.SetOutput(errOut)
	name := fs.String("name", "", "worker name (required)")
	harness := fs.String("harness", "mock", "harness: mock|codex|pi|opencode")
	prompt := fs.String("prompt", "", "optional prompt passed to the harness")
	from := fs.String("from", "", "fork from an existing worker before spawning")
	checkpoint := fs.String("checkpoint", "", "reserved: spawn from a specific manifest")
	mockTarget := fs.String("mock-target", operator.DefaultMockTarget, "MOCK_TARGET for the harness")
	mockSteps := fs.String("mock-steps", operator.DefaultMockSteps, "MOCK_STEPS for the harness")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("spawn: --name is required")
	}
	_ = *checkpoint // documented flag; snapshot restore-from-manifest is out of demo scope

	if *from != "" {
		st, err := operator.Fork(state, *name, *from, *prompt)
		if err != nil {
			return err
		}
		printSpawnResult(out, st)
		return nil
	}

	st, err := operator.Spawn(operator.SpawnConfig{
		State:      state,
		Name:       *name,
		Harness:    *harness,
		Prompt:     *prompt,
		MockTarget: *mockTarget,
		MockSteps:  *mockSteps,
	})
	if err != nil {
		return err
	}
	printSpawnResult(out, st)
	return nil
}

func printSpawnResult(out io.Writer, st *operator.Status) {
	fmt.Fprintf(out, "spawned worker %q: phase=%s backend=%s pid=%d proxy_port=%d\n",
		st.Name, st.Phase, st.SessionBackend, st.PID, st.ProxyPort)
}

func cmdSuspend(state string, args []string, out, errOut io.Writer) error {
	name, err := requireName("suspend", args, errOut)
	if err != nil {
		return err
	}
	st, err := operator.Suspend(state, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "suspended worker %q: checkpoint=%s\n", name, st.LastCheckpoint)
	return nil
}

func cmdResume(state string, args []string, out, errOut io.Writer) error {
	name, err := requireName("resume", args, errOut)
	if err != nil {
		return err
	}
	st, err := operator.Resume(state, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "resumed worker %q: backend=%s pid=%d proxy_port=%d\n",
		name, st.SessionBackend, st.PID, st.ProxyPort)
	return nil
}

func cmdFork(state string, args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	fs.SetOutput(errOut)
	name := fs.String("name", "", "new worker name (required)")
	from := fs.String("from", "", "source worker to fork (required)")
	prompt := fs.String("prompt", "", "optional prompt for the new worker")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *from == "" {
		return fmt.Errorf("fork: --name and --from are required")
	}
	st, err := operator.Fork(state, *name, *from, *prompt)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "forked %q from %q: phase=%s backend=%s pid=%d\n",
		st.Name, *from, st.Phase, st.SessionBackend, st.PID)
	return nil
}

func cmdGC(state string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	ttl := fs.Duration("ttl", time.Hour, "remove Completed workers older than this")
	if err := fs.Parse(args); err != nil {
		return err
	}
	removed, err := operator.GC(state, *ttl)
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		fmt.Fprintln(out, "gc: nothing to remove")
		return nil
	}
	fmt.Fprintf(out, "gc: removed %d worker(s): %s\n", len(removed), strings.Join(removed, ", "))
	return nil
}

// requireName parses a lone --name flag for the simple subcommands.
func requireName(cmd string, args []string, errOut io.Writer) (string, error) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(errOut)
	name := fs.String("name", "", "worker name (required)")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if *name == "" {
		return "", fmt.Errorf("%s: --name is required", cmd)
	}
	return *name, nil
}
