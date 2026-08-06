package muxwork

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/pyrex41/shenmux/orchestrator/operator"
)

func cmdList(state string, args []string, out io.Writer) error {
	if err := (flag.NewFlagSet("list", flag.ContinueOnError)).Parse(args); err != nil {
		return err
	}
	names, err := operator.ListWorkers(state)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tHARNESS\tPHASE\tBACKEND\tPID\tPROXY_PORT\tPARENT")
	for _, name := range names {
		w := operator.NewWorker(state, name)
		st, err := operator.RefreshPhase(w)
		if err != nil {
			fmt.Fprintf(tw, "%s\t?\t?\t?\t?\t?\t?\n", name)
			continue
		}
		parent := st.Parent
		if parent == "" {
			parent = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			st.Name, st.Harness, st.Phase, st.SessionBackend, st.PID, st.ProxyPort, parent)
	}
	return tw.Flush()
}

func cmdStatus(state string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	name := fs.String("name", "", "worker name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("status: --name is required")
	}
	w := operator.NewWorker(state, *name)
	if !w.Exists() {
		return fmt.Errorf("worker %q does not exist", *name)
	}
	// Reconcile phase, then print the (possibly updated) status.json.
	st, err := operator.RefreshPhase(w)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(out, string(b))
	return nil
}

func cmdLogs(state string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	name := fs.String("name", "", "worker name (required)")
	lines := fs.Int("lines", 200, "number of trailing lines to print")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("logs: --name is required")
	}
	w := operator.NewWorker(state, *name)
	// Prefer the harness transcript (the PTY content shenmux owns, mirrored to
	// a file); fall back to the session log (shenmux's own launch banner).
	src := w.TranscriptLog()
	if _, err := os.Stat(src); err != nil {
		src = w.SessionLog()
	}
	tail, err := tailFile(src, *lines)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, tail)
	return err
}

// tailFile returns the last n lines of a file (whole file if it has fewer).
func tailFile(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var ring []string
	for sc.Scan() {
		ring = append(ring, sc.Text())
		if len(ring) > n {
			ring = ring[1:]
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if len(ring) == 0 {
		return "", nil
	}
	return strings.Join(ring, "\n") + "\n", nil
}

// cmdWatch merge-tails every worker's events.ndjson and streams new lines. This
// is the demo's stand-in for the CR watch stream, and is resumable in the sense
// that events.ndjson is a durable append-only feed.
func cmdWatch(state string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit raw ndjson lines instead of human format")
	once := fs.Bool("once", false, "print the current merged feed and exit (do not follow)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	offsets := map[string]int64{}

	emit := func(ev operator.Event, raw []byte) {
		if *asJSON {
			fmt.Fprintln(out, strings.TrimRight(string(raw), "\n"))
			return
		}
		ts := time.Unix(ev.TS, 0).Format("15:04:05")
		detail := formatDetail(ev.Detail)
		if detail != "" {
			fmt.Fprintf(out, "%s  %-16s %-12s %s\n", ts, ev.Worker, ev.Event, detail)
		} else {
			fmt.Fprintf(out, "%s  %-16s %s\n", ts, ev.Worker, ev.Event)
		}
	}

	// One scan pass: read any new bytes from each worker's events.ndjson.
	scan := func() {
		names, err := operator.ListWorkers(state)
		if err != nil {
			return
		}
		type pending struct {
			ev  operator.Event
			raw []byte
		}
		var batch []pending
		for _, name := range names {
			path := operator.NewWorker(state, name).EventsPath()
			lines, newOff := readFrom(path, offsets[path])
			offsets[path] = newOff
			for _, raw := range lines {
				var ev operator.Event
				if json.Unmarshal(raw, &ev) != nil {
					continue
				}
				batch = append(batch, pending{ev: ev, raw: raw})
			}
		}
		// Order the batch by timestamp so interleaved workers read chronologically.
		sort.SliceStable(batch, func(i, j int) bool { return batch[i].ev.TS < batch[j].ev.TS })
		for _, p := range batch {
			emit(p.ev, p.raw)
		}
	}

	scan()
	if *once {
		return nil
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigc)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-sigc:
			return nil
		case <-ticker.C:
			scan()
		}
	}
}

// readFrom reads whole newline-terminated records from path starting at offset,
// returning the records (without trailing newline) and the new offset. A
// partial trailing line (no newline yet) is not consumed.
func readFrom(path string, offset int64) ([][]byte, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, offset
	}
	if fi.Size() < offset {
		// File shrank/rotated; restart from the beginning.
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset
	}
	var lines [][]byte
	r := bufio.NewReader(f)
	consumed := offset
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			consumed += int64(len(line))
			trimmed := line[:len(line)-1]
			if len(trimmed) > 0 {
				cp := make([]byte, len(trimmed))
				copy(cp, trimmed)
				lines = append(lines, cp)
			}
		}
		if err != nil {
			break
		}
	}
	return lines, consumed
}

func formatDetail(d interface{}) string {
	switch v := d.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}
