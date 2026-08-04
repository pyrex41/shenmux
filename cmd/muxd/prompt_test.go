package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareZshDemoEnvironment(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	env, cleanup, err := prepareShellEnvironment([]string{zsh, "-il"}, []string{"SHELL=/bin/bash", "NO_COLOR=1"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var zdotdir string
	for _, entry := range env {
		if strings.HasPrefix(entry, "ZDOTDIR=") {
			zdotdir = strings.TrimPrefix(entry, "ZDOTDIR=")
		}
	}
	if zdotdir == "" {
		t.Fatal("ZDOTDIR was not configured")
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "NO_COLOR=") {
			t.Fatal("NO_COLOR should not be inherited by the colorful demo shell")
		}
	}
	if !containsEnv(env, "COLORTERM=truecolor") || !containsEnv(env, "TERM_PROGRAM=shenmux") {
		t.Fatalf("TUI color capabilities missing from environment: %v", env)
	}
	contents, err := os.ReadFile(filepath.Join(zdotdir, ".zshrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "starship init zsh") {
		t.Fatal("zsh config does not include starship fallback")
	}
}

func containsEnv(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}
