# Roadmap

1. Pin and vendor a reproducible `libghostty-vt` build, then wire terminal effect callbacks that write required responses back through the guarded PTY writer.
2. Add a native full-terminal checkpoint codec when Ghostty exposes stable export/import, retaining only a bounded post-checkpoint replay log.
3. Build a graphical client that embeds libghostty, replays historical resizes off-screen, and renders the exact snapshot rather than replaying into the host terminal.
4. Add CURVE authentication, server policy for allowed client keys, and safe TCP endpoint profiles.
5. Add per-client viewport dimensions and an explicit authoritative-resize policy instead of last-writer-wins.
6. Add journal persistence, daemon restart recovery, named session discovery, and process supervision.
7. Keep Shen as the authoritative control-plane reducer: lower typed commands to state-plus-effect results, and keep PTY, terminal, ZeroMQ, clocks, persistence, and effect execution in Go adapters.
8. Replace the mux-specific bootstrap emitter with upstream `shengen` once its generated reducer/effect representations and host ABI match the required boundary.
9. Run the Bifrost suite across shen-go, shen-rust, and shen-lua in CI, with the primary Shen reducer check required by the standard build gate.
10. Restructure or specialize the session algebra so the complete reducer can run under portable Shen `tc +` checking without exhausting implementation inference budgets.
