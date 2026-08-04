# Kubernetes deployment

The controller is a single HTTP process intended to sit behind an Ingress or
other TLS reverse proxy. Agents run as sidecars and make outbound WSS
connections; no agent Service or inbound port is required.

1. Build and publish the `shenmux` image.
2. Deploy `controller.yaml`, replacing the example hostname and TLS secret.
3. Start `shenmux controller` and copy its single-use enrollment code into a
   short-lived Secret.
4. Add the sidecar fragment from `agent-sidecar.yaml` to each harness or
   orchestrator workload. Configure the harness's `muxd` with the same
   `/run/shenmux/<session>.ctl` and `.pub` endpoints shown in the fragment.
   Mount `/var/lib/shenmux` on a persistent volume if a pod restart must retain
   device identity; otherwise the pod can re-enroll.
5. Remove the bootstrap Secret after enrollment and revoke the device if the
   pod is permanently deleted.

The controller's `GET /sessions` endpoint returns metadata-only discovery
records. Browser clients request a capability for the selected `device_id` and
`session_id`, then attach through `/browser`. Terminal payloads are still
carried by the existing relay framing.

For a disposable local cluster, start the controller with
`--dev-browser-subject local-test`, port-forward its Service, and open
`http://127.0.0.1:18878/workspace?subject=local-test`. Hosted deployments must
replace this development subject with real browser authentication.

The example is intentionally a manifest fragment rather than an operator: it
works with Deployments, Jobs, StatefulSets, and custom orchestrator pods. A
future operator can automate enrollment-code rotation and revocation without
changing the transport protocol.
