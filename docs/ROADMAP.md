# Roadmap

The maintained implementation status and release gates are in
[V1/V2-PLAN.md](V1-V2-PLAN.md). In short:

- V1 is a coherent, installable local release around the implemented PTY,
  local IPC, native client, and local browser gateway.
- V2 turns the implemented development controller/agent foundation into an
  operable service by adding real browser identity, administration, durable
  storage/operations, production packaging, deployment tests, and a shipped
  blind-capable client.

The current controller, blind-stream, and direct/Tailscale code should be read
as implementation foundations, not evidence that those production gates have
been met.

Longer-term runtime work includes broader terminal compatibility, an optional
native Ghostty snapshot codec when a stable API is available, a better
per-client viewport/authoritative-resize policy, and continued tightening of
the Shen reducer boundary and portable Shen test suite.

Local-first replicated logs, offline editing, PTY migration/handoff, Golem,
and collaborative conflict resolution are later research topics. They are not
part of the current runtime or V1/V2 deployment contract.
