# Trust model

## Trusted computing base

The structural trusted computing base is:

- `specs/mux.shen`;
- `cmd/shenmux-gen` and `codegen/guards_gen.go.tmpl`;
- the generated Go-facing reducer/effect boundary after drift verification;
- the reducer/effect contract tests that pin command, state, rejection, and effect semantics;
- Go's type checker and runtime;
- the small C ABI declarations in `internal/zmqx`, `internal/ptyx`, and optional `internal/term/ghostty.go`;
- libzmq, the OS PTY implementation, and libghostty-vt when enabled.

The mux-specific emitter is intentionally narrower than generic `shengen`. It checks for exactly one declaration of every required datatype and reducer transition form, hashes the full Shen source, and generates a closed Go boundary. A change outside the recognized shape fails regeneration instead of silently producing permissive code. The emitter and Shen test suite are complementary: generated code preserves the typed Go ABI, while the Shen reducer tests establish the legal transition and effect traces. The portable trace suite currently loads the annotated reducer under `tc -`; current Shen ports exhaust their inference budget on the heterogeneous session reducer under `tc +`. Neither gate is a formal proof of the Go effect executor or of arbitrary Shen programs.

## Structurally enforced

- Client IDs and dimensions cross host boundaries through validated constructors.
- Sequence values are opaque and advance through `NextSeq`.
- Session fields are private.
- Attach, detach, snapshot lock, input authorization, event sequencing, rejection reasons, and effect selection go through the Shen reducer transition boundary.
- Raw `Session` literals outside the generated reducer boundary fail the audit gate.
- The wire decoder bounds frames and rejects unknown protocol versions and kinds.
- Snapshot decoding validates exact sequence continuity and record shape.

## Runtime-checked assumptions

- ROUTER identity is the client identity. Default filesystem IPC sockets are reachable only through a daemon-owned directory whose group/other permission bits are clear.
- A daemon owns each filesystem endpoint only after acquiring its persistent `0600` advisory lock sentinel; stale socket removal occurs after the lock, so a competing process cannot unlink a live endpoint.
- A client has subscribed before attach because XPUB observed `ready/<id>`.
- Terminal, journal, and model mutations occur under the runtime mutex; PTY writes are handed to the single writer actor and execute outside that lock. A reducer result is committed only after required synchronous impure effects succeed.
- Resize failures restore the PTY when possible; failures after terminal reflow close the runtime and emit a fatal consistency error rather than publishing an inconsistent state.
- Each ZeroMQ socket has exactly one goroutine owner.
- The libzmq 4.x ABI represents `zmq_msg_t` as the documented 64-byte opaque value.
- The pinned libghostty-vt headers match the linked library when the build tag is enabled.

## Current prototype limitations

The local muxd/WebSocket prototype remains intentionally local-only. The
authenticated controller path now has durable ACL/capability/lease/audit
policy, optional direct/Tailscale transport, and an endpoint-integrated blind
stream: the native blind-stream client helper performs the X25519/Ed25519
handshake, the agent decrypts and validates inner frames, and all
attached/delta/input payloads are encrypted before crossing the relay. The
controller continues to see only routing metadata and ciphertext for blind
streams. Owner-only local IPC containment is not a substitute for
cryptographic identity; hosted deployments still need a real browser identity
provider.

## Not claimed

- V1 trusted-server relay does not provide blind end-to-end confidentiality.
- Blind relay confidentiality is a V2 claim scoped to an honest identity
  provider, browser origin/client distribution, and endpoint keys. Key
  exchange, downgrade, replay, recovery, and bridge integration are covered;
  routing, timing, and size metadata still remain visible to the controller.
- No durable delivery guarantee from PUB/SUB.
- No proof of liveness or bounded memory for an indefinitely running session.
- No full native terminal snapshot in the current Ghostty C API integration.
- No guarantee that the basic metadata tracker emulates terminal cells; it does not.
- No formal proof of the Go effect executor against the Shen reducer beyond the generated boundary, trace/contract tests, and runtime integration tests.

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

`make check` runs `./scripts/shen-check.sh` as a required gate. It fails when
neither Bifrost nor a configured Shen launcher is available. In CI, install the
specific Shen implementation used by the project (or set `SHEN_BIN`) before
running the gate; local development should use the same pinned environment.

For the complete cross-implementation suite, also run:

```sh
make bifrost
sb gates
```
