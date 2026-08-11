# Deployment examples

These files illustrate intended process shapes; they are not a supported or
tested distribution.

- `systemd/` sketches separate controller and outbound-agent services.
- `shenmux-entrypoint` is the container entrypoint; it starts the legacy `muxd`
  daemon, so running a controller or agent from the image means overriding it.

They assume real TLS and browser identity integration, durable state paths
appropriate to the host, and an enrollment workflow. The root `Dockerfile`
builds `shenmux` alongside `muxd` and `muxctl`.

Review the limitations and production checklist in
[`docs/DEPLOYMENT.md`](../docs/DEPLOYMENT.md) before adapting any file.
