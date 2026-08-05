# Kubernetes architecture example

The manifests in this directory are fragments for development and design
review, not a deployable release. They assume a container image containing the
`shenmux` binary; the root Dockerfile does not currently build that image.

The intended process shape is:

1. A controller sits behind an Ingress that supplies TLS and verified browser
   identity.
2. An agent sidecar opens an outbound WebSocket to that controller.
3. A separate `shenmux run`/`muxd` process in the workload owns the PTY and
   shares an owner-only IPC volume with the sidecar.
4. The agent advertises session/workload metadata and bridges an authorized
   stream to matching local IPC endpoints.

Before adapting the examples, fix at least these deliberate omissions:

- mount writable durable controller state and pass `--state-dir`;
- pass the sidecar's `/var/lib/shenmux` mount as `--state-dir` or
  `SHENMUX_STATE_DIR`;
- provide the actual PTY-owning process and matching control/data endpoints;
- replace the development browser subject with real authentication and Origin
  enforcement;
- remove the enrollment Secret after first use and add rotation/revocation
  operations;
- select a private IPC ownership model that works with the pod's users and
  security contexts;
- add resource limits, network policy, monitoring, backups, and tested upgrade
  behavior.

`GET /sessions` returns only metadata advertised by currently connected
agents. It is memory-only presence, not durable discovery or proof that a
local PTY is healthy.

See [`docs/DEPLOYMENT.md`](../../docs/DEPLOYMENT.md) for the current production
gaps.
