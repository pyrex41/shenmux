package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateEmbedsSemanticSourceAndRejectsMissingFunction(t *testing.T) {
	root := filepath.Join("..", "..")
	spec, err := os.ReadFile(filepath.Join(root, "specs", "mux.shen"))
	if err != nil {
		t.Fatal(err)
	}
	template, err := os.ReadFile(filepath.Join(root, "codegen", "guards_gen.go.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	generated, hash, err := generate(spec, template)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(generated), hash) || !strings.Contains(string(generated), "mux.accept-input?") {
		t.Fatal("generated guards do not embed hash and model source")
	}
	broken := strings.Replace(string(spec), "(define mux.apply-exit", "(define mux.apply-exit-missing", 1)
	if _, _, err := generate([]byte(broken), template); err == nil {
		t.Fatal("generator accepted a missing semantic function")
	}
	broken = strings.Replace(string(spec), "(define mux.reduce\n", "(define mux.reduce-missing\n", 1)
	if _, _, err := generate([]byte(broken), template); err == nil {
		t.Fatal("generator accepted a missing reducer")
	}
}
