package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const demoZshrc = `# shenmux demo prompt; this file is session-local and never touches ~/.zshrc.
setopt prompt_subst
if (( $+commands[starship] )); then
  eval "$(starship init zsh)"
else
  autoload -Uz colors && colors
  PROMPT='%B%F{cyan}%n%f%b%F{8}@%m%f %B%F{blue}%~%f%b %(?..%F{red}[%?]%f )%F{green}❯%f '
  RPROMPT='%F{8}shenmux%f'
fi
`

const demoFishConfig = `# shenmux demo prompt; this file is session-local and never touches ~/.config.
if type -q starship
  starship init fish | source
else
  function fish_prompt
    set_color cyan; echo -n (whoami)'@'(hostname)' '
    set_color blue; echo -n (prompt_pwd)' '
    set_color green; echo -n '❯ '
    set_color normal
  end
end
`

func prepareShellEnvironment(command, env []string) ([]string, func(), error) {
	if len(command) == 0 {
		return env, func() {}, nil
	}
	shell := filepath.Base(command[0])
	env = withEnv(env, "TERM", "xterm-256color")
	// Advertise the capabilities expected by modern TUIs (including Claude
	// Code, fzf, and full-screen editors) without changing the user's shell.
	env = withEnv(env, "COLORTERM", "truecolor")
	env = withEnv(env, "TERM_PROGRAM", "shenmux")
	// NO_COLOR is useful for normal command-line tools, but it makes a remote
	// terminal demo look broken and causes TUI clients to suppress ANSI output.
	env = withoutEnv(env, "NO_COLOR")
	if shell != "zsh" && shell != "fish" {
		return env, func() {}, nil
	}
	root, err := os.MkdirTemp("", "shenmux-demo-")
	if err != nil {
		return nil, nil, fmt.Errorf("create shell config: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	set := func(key, value string) { env = withEnv(env, key, value) }
	switch shell {
	case "zsh":
		if err := os.WriteFile(filepath.Join(root, ".zshrc"), []byte(demoZshrc), 0o600); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("write zsh demo config: %w", err)
		}
		set("ZDOTDIR", root)
	case "fish":
		configDir := filepath.Join(root, "fish")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("create fish demo config: %w", err)
		}
		if err := os.WriteFile(filepath.Join(configDir, "config.fish"), []byte(demoFishConfig), 0o600); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("write fish demo config: %w", err)
		}
		set("XDG_CONFIG_HOME", root)
	}
	return env, cleanup, nil
}

func withEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

func withoutEnv(env []string, key string) []string {
	prefix := key + "="
	out := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}
