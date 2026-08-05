# Trust model

The practical trust boundary depends on which interface you use. This document
separates controls present in the code from security a deployment must add.

## Local session

`shenmux run` trusts processes running as the same operating-system user.
Default IPC sockets are placed in an owner-only directory, and advisory lock
sentinels prevent a second daemon from unlinking live endpoints. Custom IPC
parents are rejected unless they have the same ownership and permission shape.

Inside that boundary, protocol decoders bound frame sizes and validate
versions, kinds, client IDs, session names, dimensions, sequence numbers, and
archives. The ROUTER identity is the effective client identity. The
Shen-derived reducer authorizes attach, control ownership, input, resize,
detach, and event sequencing before Go executes effects.

This is local containment, not cryptographic authentication. Do not publish
the ZeroMQ endpoints or make their directory available to other users. The
local browser gateway also has no authentication and should remain bound to
loopback.

## Development controller identity

Agent enrollment uses a random, single-use code that expires after ten
minutes. The controller stores only its hash until it is consumed, then issues
a device ID and random credential. The agent also creates an Ed25519 key pair
and proves possession during each WebSocket challenge. Controller enrollment
and policy files are written owner-only and atomically.

This implementation has important limits:

- Device credentials have a 30-day expiry value, but automatic rotation and
  re-enrollment operations are not implemented.
- The controller command has no OIDC/OAuth/session integration. Its fallback
  accepts `X-Shenmux-Subject`, and development mode can accept a matching
  `?subject=` query parameter. Neither authenticates a public user by itself.
- The WebSocket upgrader currently accepts any Origin. A production edge must
  enforce origin policy.
- The listener is plain HTTP. HTTPS/WSS requires a reverse proxy or other TLS
  termination that is not configured by shenmux.
- There is no command-line or web administrator workflow for grants,
  revocation, audit review, credential rotation, or recovery.

Browser attach capabilities are random bearer values stored by the policy
store. They are subject/device/session/permission bound, short lived (one
minute by default), and accepted for only one new stream by the controller.
Trusted-mode input and resize also require a control capability and a renewable
controller lease. Revocation helpers and policy tests exist in the Go package,
but the `shenmux controller` command does not expose an operations API for
them.

## Trusted relay mode

Trusted mode is the default and is what the bundled controller workspace uses.
The controller decodes inner session messages to enforce command type,
capability permission, session binding, and control lease. Consequently, the
controller can read terminal contents and input. HTTPS/WSS, when supplied by a
deployment, protects only the network hops.

Choose trusted mode only when the controller operator and its storage/runtime
are allowed to see session data.

## Blind relay mode

The relay and agent packages implement an endpoint-side blind stream:

- the client and enrolled agent bind device, session, stream, subject,
  permission, and capability digest into the handshake;
- X25519 supplies ephemeral agreement, the agent Ed25519 key authenticates the
  transcript, HKDF derives keys, and AEAD protects inner frames;
- counters and epochs protect nonce use and reject replay;
- the controller forwards routing headers and ciphertext rather than decoded
  terminal frames.

The controller still sees device/session/stream routing, frame sizes, timing,
and connection metadata. It still authenticates the browser identity, issues
the capability, and distributes the agent public identity. The claim therefore
assumes an honest identity provider and an authentic blind client. A controller
that can replace browser code or substitute identities remains in a position
to attack the session.

Most importantly, the repository does not ship a browser or native executable
that initiates this blind handshake. The primitives and integration tests are
available to developers, but blind relay is not an end-user feature of the
bundled workspace.

Persisting `--trust-mode blind` sets the agent's minimum accepted stream mode.
It rejects the bundled trusted workspace rather than silently downgrading.

## State exposure

Protect these files like credentials:

- user config and agent state under the resolved XDG paths;
- controller `enrollment.json` and `policy.json` under `--state-dir`.

The agent state contains the device private key and bearer credential. The
controller enrollment file contains device public keys and bearer credentials.
The policy file contains grants, capabilities, leases, revocations, and audit
metadata. Terminal payloads are not intentionally written to those files.

Terminal checkpoints and deltas are memory-only in the current runtime. A
process crash can still expose them through ordinary process/core-dump or host
administrator access; blind mode does not protect compromised endpoints.

## Implementation assurance

The mux-specific trusted computing base includes:

- `specs/mux.shen`, the generator, template, and generated guard boundary;
- reducer/effect contract and trace tests;
- the Go effect executor and runtime;
- protocol, relay, policy, PTY, terminal, and ZMTP adapters;
- Go, the operating system, and optional libghostty-vt.

Generated types make guarded IDs, dimensions, and sequence values hard to
construct incorrectly, and the audit gate prevents raw session literals
outside the reducer boundary. This is not a formal proof of the Go executor,
the complete system, liveness, or bounded memory for an indefinitely running
session.

## Explicit non-claims

- No public controller or browser authentication is supplied.
- No confidentiality from the controller in trusted mode.
- No end-user blind client is supplied.
- No durable delivery guarantee from ZeroMQ PUB/SUB; gaps recover by resync.
- No persistence of a live PTY or terminal checkpoint across daemon/host
  restart.
- No complete terminal-emulation compatibility guarantee.
- No Windows PTY support.
- No reviewed production datastore, rate-limiting layer, multi-node
  coordination, or disaster-recovery implementation.

## Verification commands

```sh
./scripts/shengen-codegen.sh --check
./scripts/shenguard-audit.sh
./scripts/shen-check.sh
go test ./...
go test -race ./...
go vet ./...
go build ./...
CGO_ENABLED=0 go build ./...
```

`make check` includes the generated-code, audit, Shen, test, vet, web-build,
and build gates. The Shen gate requires Bifrost or a configured `SHEN_BIN`.
For the broader portable suite, also run `make bifrost` and `sb gates`.
