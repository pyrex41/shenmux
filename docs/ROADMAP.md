# Roadmap

1. Pin and vendor a reproducible `libghostty-vt` build, then wire terminal effect callbacks that write required responses back through the guarded PTY writer.
2. Add a native full-terminal checkpoint codec when Ghostty exposes stable export/import, retaining only a bounded post-checkpoint replay log.
3. Build a graphical client that embeds libghostty, replays historical resizes off-screen, and renders the exact snapshot rather than replaying into the host terminal.
4. Add CURVE authentication, server policy for allowed client keys, and safe TCP endpoint profiles.
5. Add per-client viewport dimensions and an explicit authoritative-resize policy instead of last-writer-wins.
6. Add journal persistence, daemon restart recovery, named session discovery, and process supervision.
7. Replace the mux-specific bootstrap emitter with upstream `shengen` once its generated host representations and transition lowering match the required ABI.
8. Run the Bifrost suite across shen-go, shen-rust, and shen-lua in CI.
