# Deployment examples

`systemd/shenmux-agent.service` is the shared service contract for EC2,
Hetzner, and home servers. Set `SHENMUX_CONTROLLER` in a root-readable
environment file, enroll once, and persist `/var/lib/shenmux`.

`fly.toml.example` is an egress-only Fly Machine template. It mounts a volume
for the device identity and deliberately publishes no agent ports.

The controller remains the only public HTTPS/WSS service. See
[`docs/DEPLOYMENT.md`](../docs/DEPLOYMENT.md) for firewall, storage, and
outage/reconnect requirements.
