# Deployment contracts

The agent is deliberately an egress-only service. It needs DNS and outbound
TCP 443 to the controller; it does not expose ZeroMQ, SSH, or an HTTP port.
Persist the state directory so the device key survives upgrades and reboots.

## Local-only

Run `shenmux run` (or `muxd` for compatibility) and use `shenmux web` on the
same host. No account, controller, or network access is required. The Unix
control/data sockets must remain owner-only.

## Hosted controller

Run `shenmux agent --controller https://controller.example` on the host that
owns the PTY. Enroll once with a short-lived code, then keep
`/var/lib/shenmux` (or `$XDG_STATE_HOME/shenmux`) on durable storage. The
browser connects only to the controller's HTTPS/WSS endpoint.

Run the controller with `shenmux controller --state-dir
/var/lib/shenmux-controller`. That directory contains the owner-only durable
enrollment/device credential store and policy database; back it up together
with its TLS and identity-provider configuration. Development controllers may
use the default XDG state path, but production deployments should set the
directory explicitly on a persistent volume.

## Fly.io

Use a Machine volume for `/var/lib/shenmux` and allow egress DNS/HTTPS. The
agent needs no `[http_service]` or public listener:

```toml
app = "replace-me"
primary_region = "ord"

[build]
  image = "ghcr.io/pyrex41/shenmux:latest"

[mounts]
  source = "shenmux_state"
  destination = "/var/lib/shenmux"

[processes]
  agent = "shenmux agent --controller https://controller.example"
```

Create the volume in the target region and provide the enrollment code as a
one-time secret. Do not publish the agent's control/data ports.

## AWS EC2

Attach an EBS volume at `/var/lib/shenmux`, install
`deploy/systemd/shenmux-agent.service`, and set `SHENMUX_CONTROLLER` in an
environment drop-in. The security group requires only outbound TCP 443 and
DNS; no inbound rule is needed for the agent. Put the controller behind an
HTTPS load balancer or use the hosted service.

## Hetzner

Mount a persistent Volume at `/var/lib/shenmux`, install the same systemd unit,
and permit outbound 443/DNS in the firewall. NAT is supported. Keep the
controller URL and enrollment code out of the unit file (use an environment
file with mode 0600).

## Home server

Install the binary and `deploy/systemd/shenmux-agent.service` (or a launchd
equivalent on macOS), persist the state directory, and enroll from the
controller. No port forwarding is required; the agent maintains the outbound
WSS connection through NAT. Local Unix IPC remains available during a
controller outage.

All deployments should expose the controller's `/healthz` endpoint and back up
controller state. A reconnecting browser receives a fresh checkpoint followed
by ordered deltas; it never relies on replaying an unbounded browser queue.
