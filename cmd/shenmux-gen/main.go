// shenmux-gen lowers the restricted Shen model into opaque Go guards. The
// generated package embeds the exact Shen source and evaluates every pure
// predicate and transition through internal/shenmodel; the template contains
// host representations and boundary checks, not duplicated transition bodies.
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pyrex41/shenmux/internal/shenmodel"
)

var requiredDatatypes = []string{"client-id", "seq-no", "dimensions", "snapshot", "session"}

var requiredFunctions = map[string]int{
	"mux.valid-client-id?":     1,
	"mux.valid-dimensions?":    2,
	"mux.valid-snapshot-meta?": 5,
	"mux.member?":              2,
	"mux.remove-client":        2,
	"mux.release-if-owner":     2,
	"mux.attach-ok?":           2,
	"mux.has-control?":         2,
	"mux.accept-input?":        2,
	"mux.accept-resize?":       2,
	"mux.acquire-control-ok?":  2,
	"mux.release-control-ok?":  2,
	"mux.begin-snapshot":       1,
	"mux.end-snapshot":         2,
	"mux.attach":               2,
	"mux.detach":               2,
	"mux.acquire-control":      2,
	"mux.release-control":      2,
	"mux.next-seq":             1,
	"mux.event-ok?":            2,
	"mux.apply-delta":          2,
	"mux.apply-resize":         3,
	"mux.apply-control":        2,
	"mux.apply-exit":           2,
	"mux.reduce":               2,
}

func main() {
	specPath := flag.String("spec", "specs/mux.shen", "Shen specification")
	templatePath := flag.String("template", "codegen/guards_gen.go.tmpl", "Go emitter template")
	outPath := flag.String("out", "internal/shenguard/guards_gen.go", "generated Go output")
	check := flag.Bool("check", false, "fail if output differs instead of writing")
	flag.Parse()

	spec, err := os.ReadFile(*specPath)
	fatalIf(err)
	template, err := os.ReadFile(*templatePath)
	fatalIf(err)
	formatted, hash, err := generate(spec, template)
	fatalIf(err)

	if *check {
		current, err := os.ReadFile(*outPath)
		fatalIf(err)
		if !bytes.Equal(current, formatted) {
			fatalf("generated guards are stale; run go generate ./internal/shenguard")
		}
		fmt.Printf("PASS: %s is executable lowering of %s (sha256 %s)\n", *outPath, *specPath, hash)
		return
	}
	fatalIf(os.MkdirAll(filepath.Dir(*outPath), 0o755))
	fatalIf(os.WriteFile(*outPath, formatted, 0o644))
	fmt.Printf("generated %s from executable Shen model %s (sha256 %s)\n", *outPath, *specPath, hash)
}

func generate(spec, template []byte) ([]byte, string, error) {
	program, err := shenmodel.Parse(string(spec))
	if err != nil {
		return nil, "", fmt.Errorf("parse Shen model: %w", err)
	}
	for _, name := range requiredDatatypes {
		if !program.HasDatatype(name) {
			return nil, "", fmt.Errorf("Shen model is missing datatype %s", name)
		}
	}
	for name, expectedArity := range requiredFunctions {
		arity, ok := program.FunctionArity(name)
		if !ok {
			return nil, "", fmt.Errorf("Shen model is missing function %s", name)
		}
		if arity != expectedArity {
			return nil, "", fmt.Errorf("Shen function %s has arity %d, expected %d", name, arity, expectedArity)
		}
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(spec))
	generated := string(template)
	replacements := map[string]string{
		"{{SPEC_SHA256}}": hash,
		"{{SPEC_SOURCE}}": strconv.Quote(string(spec)),
	}
	for placeholder, replacement := range replacements {
		if !strings.Contains(generated, placeholder) {
			return nil, "", fmt.Errorf("template is missing placeholder %s", placeholder)
		}
		generated = strings.ReplaceAll(generated, placeholder, replacement)
	}
	if strings.Contains(generated, "{{SPEC_") {
		return nil, "", fmt.Errorf("template contains unexpanded spec placeholder")
	}
	formatted, err := format.Source([]byte(generated))
	if err != nil {
		return nil, "", fmt.Errorf("format generated guards: %w", err)
	}
	return formatted, hash, nil
}

func fatalIf(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "shenmux-gen: "+format+"\n", args...)
	os.Exit(1)
}
