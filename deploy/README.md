# Deployment examples

These files illustrate intended process shapes; they are not a supported or
tested distribution.

- `systemd/` sketches separate controller and outbound-agent services.
- `fly.toml.example` sketches an egress-only agent with durable state.
- `kubernetes/` sketches a controller and an agent sidecar sharing local IPC
  with a separately running session daemon.
- `kubernetes/orchestrator/` sketches the multi-harness agent-worker
  orchestration layer designed in
  [`docs/K8S-ORCHESTRATION.md`](../docs/K8S-ORCHESTRATION.md): an
  `AgentWorker` CRD and the per-worker pod shape for durable, resumable,
  forkable coding-agent workers (claude-managed, codex, pi, opencode)
  observable through shenmux.

They assume an image containing the `shenmux` binary, real TLS and browser
identity integration, corrected durable state paths, and an enrollment
workflow. The root `Dockerfile` currently builds only the legacy `muxd` image,
so it cannot run these controller/agent examples.

Review the limitations and production checklist in
[`docs/DEPLOYMENT.md`](../docs/DEPLOYMENT.md) before adapting any file.
