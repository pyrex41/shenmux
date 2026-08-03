# Trust model

## Trusted computing base

The structural trusted computing base is:

- `specs/mux.shen`;
- `cmd/shenmux-gen` and `codegen/guards_gen.go.tmpl`;
- `internal/shenguard/guards_gen.go` after drift verification;
- Go's type checker and runtime;
- the small C ABI declarations in `internal/zmqx`, `internal/ptyx`, and optional `internal/term/ghostty.go`;
- libzmq, the OS PTY implementation, and libghostty-vt when enabled.

The mux-specific emitter is intentionally narrower than generic `shengen`. It checks for exactly one declaration of every required datatype and transition form, hashes the full Shen source, and generates a closed guard package. A change outside the recognized shape fails regeneration instead of silently producing permissive code. It does not constitute a proof that the template perfectly lowers arbitrary Shen; that mapping is a named TCB assumption until the generic emitter represents these host-specific integer choices directly.

## Structurally enforced

- Client IDs and dimensions cross host boundaries through validated constructors.
- Sequence values are opaque and advance through `NextSeq`.
- Session fields are private.
- Attach, detach, snapshot lock, input authorization, and event sequencing go through transition functions.
- Raw `Session` literals outside the generated guard package fail the audit gate.
- The wire decoder bounds frames and rejects unknown protocol versions and kinds.
- Snapshot decoding validates exact sequence continuity and record shape.

## Runtime-checked assumptions

- ROUTER identity is the client identity. Default filesystem IPC sockets are reachable only through a daemon-owned directory whose group/other permission bits are clear.
- A daemon owns each filesystem endpoint only after acquiring its persistent `0600` advisory lock sentinel; stale socket removal occurs after the lock, so a competing process cannot unlink a live endpoint.
- A client has subscribed before attach because XPUB observed `ready/<id>`.
- PTY, terminal, journal, and model mutations occur under the runtime mutex.
- Each ZeroMQ socket has exactly one goroutine owner.
- The libzmq 4.x ABI represents `zmq_msg_t` as the documented 64-byte opaque value.
- The pinned libghostty-vt headers match the linked library when the build tag is enabled.

## Not claimed

- No remote authentication or authorization policy; owner-only local IPC containment is not a substitute for cryptographic identity.
- No encrypted transport configuration.
- No durable delivery guarantee from PUB/SUB.
- No proof of liveness or bounded memory for an indefinitely running session.
- No full native terminal snapshot in the current Ghostty C API integration.
- No guarantee that the basic metadata tracker emulates terminal cells; it does not.
- No formal proof of the Go runtime against the Shen functions beyond the generated structural boundary and tests.

## Verification commands

```sh
./scripts/shengen-codegen.sh --check
./scripts/shenguard-audit.sh
go test ./...
go test -race ./...
go vet ./...
go build ./...
CGO_ENABLED=0 go build ./...
```

With Shen/Bifrost installed, also run:

```sh
./scripts/shen-check.sh
sb gates
```
