package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func defaultShellCommand(preference string) ([]string, error) {
	preference = strings.TrimSpace(preference)
	if preference == "" {
		preference = "auto"
	}
	candidates := []string{preference}
	if preference == "auto" {
		// zsh is the macOS default; fish is a pleasant fallback when it is
		// installed. A user's explicit fish/zsh SHELL still wins.
		candidates = nil
		if shell := os.Getenv("SHELL"); shell != "" {
			base := filepath.Base(shell)
			if base == "fish" || base == "zsh" {
				candidates = append(candidates, shell)
			}
		}
		candidates = append(candidates, "zsh", "fish")
		if shell := os.Getenv("SHELL"); shell != "" {
			candidates = append(candidates, shell)
		}
		candidates = append(candidates, "bash", "sh")
	}
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		return []string{path, "-il"}, nil
	}
	return nil, fmt.Errorf("no usable shell found for -shell %q", preference)
}
