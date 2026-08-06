// Command snapshot is the CLI for content-addressed overlay checkpoints with an
// autopoiesis-compatible manifest. Subcommands:
//
//	snapshot scan        --dir DIR
//	snapshot checkpoint  --workspace DIR --store STORE --manifest OUT [--parent ID]
//	snapshot restore     --manifest FILE --store STORE --into DIR
//	snapshot fork        --manifest FILE --store STORE --into DIR
//	snapshot diff        --a MANIFEST --b MANIFEST
//	snapshot try-overlay --lower L --upper U --work W --merged M
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/pyrex41/shenmux/orchestrator/snapshot"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "scan":
		err = cmdScan(os.Args[2:])
	case "checkpoint":
		err = cmdCheckpoint(os.Args[2:])
	case "restore":
		err = cmdRestore(os.Args[2:])
	case "fork":
		err = cmdFork(os.Args[2:])
	case "diff":
		err = cmdDiff(os.Args[2:])
	case "try-overlay":
		err = cmdTryOverlay(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `snapshot - content-addressed overlay checkpoints

usage:
  snapshot scan        --dir DIR
  snapshot checkpoint  --workspace DIR --store STORE --manifest OUT [--parent ID]
  snapshot restore     --manifest FILE --store STORE --into DIR
  snapshot fork        --manifest FILE --store STORE --into DIR
  snapshot diff        --a MANIFEST --b MANIFEST
  snapshot try-overlay --lower L --upper U --work W --merged M
`)
}

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	dir := fs.String("dir", "", "directory to scan")
	fs.Parse(args)
	if *dir == "" {
		return fmt.Errorf("--dir is required")
	}
	entries, root, err := snapshot.Scan(*dir, nil)
	if err != nil {
		return err
	}
	for _, e := range entries {
		fmt.Printf("%s\t%s\tmode=%d\tsize=%d\tmtime=%d\n", e.Path, e.Hash, e.Mode, e.Size, e.Mtime)
	}
	fmt.Printf("tree-hash %s\n", root)
	return nil
}

func cmdCheckpoint(args []string) error {
	fs := flag.NewFlagSet("checkpoint", flag.ExitOnError)
	workspace := fs.String("workspace", "", "workspace directory")
	store := fs.String("store", "", "content-addressed store directory")
	manifest := fs.String("manifest", "", "output manifest path")
	parent := fs.String("parent", "", "optional parent snapshot id")
	fs.Parse(args)
	if *workspace == "" || *store == "" || *manifest == "" {
		return fmt.Errorf("--workspace, --store and --manifest are required")
	}
	id, err := snapshot.Checkpoint(*workspace, *store, *manifest, *parent)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	manifest := fs.String("manifest", "", "manifest file")
	store := fs.String("store", "", "content-addressed store directory")
	into := fs.String("into", "", "destination directory")
	fs.Parse(args)
	if *manifest == "" || *store == "" || *into == "" {
		return fmt.Errorf("--manifest, --store and --into are required")
	}
	if err := snapshot.Restore(*manifest, *store, *into); err != nil {
		return err
	}
	fmt.Printf("restored into %s\n", *into)
	return nil
}

func cmdFork(args []string) error {
	fs := flag.NewFlagSet("fork", flag.ExitOnError)
	manifest := fs.String("manifest", "", "source manifest file")
	store := fs.String("store", "", "content-addressed store directory")
	into := fs.String("into", "", "destination directory")
	fs.Parse(args)
	if *manifest == "" || *store == "" || *into == "" {
		return fmt.Errorf("--manifest, --store and --into are required")
	}
	newPath, err := snapshot.Fork(*manifest, *store, *into)
	if err != nil {
		return err
	}
	fmt.Println(newPath)
	return nil
}

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	a := fs.String("a", "", "manifest A")
	b := fs.String("b", "", "manifest B")
	fs.Parse(args)
	if *a == "" || *b == "" {
		return fmt.Errorf("--a and --b are required")
	}
	changes, err := snapshot.Diff(*a, *b)
	if err != nil {
		return err
	}
	for _, c := range changes {
		switch c.Kind {
		case "added":
			fmt.Printf("+ %s\n", c.Path)
		case "removed":
			fmt.Printf("- %s\n", c.Path)
		case "modified":
			fmt.Printf("~ %s\n", c.Path)
		}
	}
	if len(changes) == 0 {
		fmt.Println("(no differences)")
	}
	return nil
}

func cmdTryOverlay(args []string) error {
	fs := flag.NewFlagSet("try-overlay", flag.ExitOnError)
	lower := fs.String("lower", "", "lowerdir")
	upper := fs.String("upper", "", "upperdir")
	work := fs.String("work", "", "workdir")
	merged := fs.String("merged", "", "merged mountpoint")
	fs.Parse(args)
	if *lower == "" || *upper == "" || *work == "" || *merged == "" {
		return fmt.Errorf("--lower, --upper, --work and --merged are required")
	}
	used, msg, err := snapshot.TryOverlay(*lower, *upper, *work, *merged)
	if err != nil {
		return err
	}
	if used {
		fmt.Printf("overlay: ENABLED - %s\n", msg)
	} else {
		fmt.Printf("overlay: FALLBACK - %s\n", msg)
	}
	// Graceful degradation: always exit 0 from try-overlay.
	return nil
}
