# shenmux build report

Build date: 2026-08-01

Target: Linux x86-64

Status: working prototype; Go path fully built and exercised

## Result

This repository implements the terminal multiplexer described by the project notes:

- Shen is the human-edited source of truth for protocol values and pure session transitions.
- Generated Go guards expose opaque values and immutable transition functions.
- A single daemon owns the Unix PTY master and terminal state.
- ZeroMQ ROUTER/DEALER serializes attach, input, resize, detach, ping, and resynchronization.
- ZeroMQ XPUB/SUB broadcasts one ordered stream of PTY bytes, resizes, and process exit.
- Attach uses an XPUB subscription barrier before returning a targeted snapshot.
- Snapshots are bounded, gzip-compressed replay archives with exact sequence validation.
- A reusable Go client and two binaries, `muxd` and `muxctl`, are included.
- An optional `libghostty-vt` adapter is isolated behind the `libghostty` build tag.

## Deliberate corrections to the initial sketch

1. **ROUTER/DEALER replaces PUSH/PULL for control.** It preserves one serialized writer while providing authoritative routing identities and targeted attach/resync/error responses.
2. **XPUB replaces PUB.** XPUB lets the daemon observe the client's unique readiness subscription and avoid the normal PUB/SUB slow-joiner loss during attach.
3. **One sequence covers all observable state changes.** PTY bytes, authoritative resize, and child exit share the same monotonic sequence.
4. **Snapshots use ordered replay today.** The inspected public Ghostty C API supports terminal construction, VT writes, resize, and state reads, but no stable full-terminal export/import facility was available. The snapshot codec is therefore replaceable without pretending that an unavailable native serializer exists.
5. **ZMQ remains a Go impure edge.** A minimal Cgo binding uses the stable libzmq C ABI and does not require ZeroMQ headers on Linux. Shen does not execute on the byte hot path.
6. **Filesystem IPC ownership is race-safe.** Default sockets live in a verified owner-only per-UID directory, and persistent `0600` advisory lock sentinels are acquired before stale socket removal or bind.

## Verification performed

All of the following completed successfully in the final source tree:

```text
./scripts/shengen-codegen.sh --check
./scripts/shenguard-audit.sh
go test ./...
go vet ./...
go build ./...
CGO_ENABLED=0 go build ./...
go test -race ./...
go test -shuffle=on -count=5 ./...
make build
```

The generated guard package matched `specs/mux.shen` at SHA-256:

```text
87b2e9bf58803aea0a0f135ba964fd5f54af54322f641345788716abfb09014d
```

The test suite includes unit coverage for guarded transitions, protocol framing, snapshot limits, the PTY, the headerless ZeroMQ binding, endpoint validation and ownership locks, cancellation/reaping, actor shutdown, and client behavior. Its integration test launches `/bin/sh` under `openpty`, attaches a real DEALER/SUB client over filesystem IPC, writes through ROUTER, receives ordered output over XPUB, and observes process exit.

## Standalone binary smoke test

A final smoke test ran the built binaries around this child command:

```sh
sh -c 'IFS= read -r x; printf "got:%s\n" "$x"'
```

Observed result:

```text
client input:              hello\n
client output bytes:       \x1b[2J\x1b[Hhello\r\ngot:hello\r\n
client exit code:          0
daemon exit code:          0
duplicate daemon code:     1
per-UID directory mode:    0700
control lock mode:         0600
data lock mode:            0600
```

The duplicate daemon failed before disturbing the live endpoints.

## Build environment

```text
OS:       Linux 6.12.13 x86-64
Go:       go1.23.2 linux/amd64
Cgo:      enabled
libzmq:   4.3.5, SONAME libzmq.so.5
```

The Linux binaries are dynamically linked and require a compatible glibc system plus `libzmq.so.5` and its runtime dependencies.

## Wired but not executable in this environment

### Shen and Bifrost

`specs/mux.shen`, `tests/mux-spec.shen`, `bifrost.suite.json`, `sb.toml`, and the Shen/guard scripts are included. The Shen/Bifrost execution gate could not be run because no `bifrost`, `sb`, `shengen`, `shen-go`, or other Shen launcher was installed, and network installation was unavailable. The script fails closed with an actionable error rather than reporting a false pass.

### libghostty-vt

The `libghostty` build-tag adapter is included, but the environment had no Ghostty headers or library. A tagged build therefore correctly failed at the missing `ghostty/vt/terminal.h`. The default build and all runtime tests use the replay-backed basic terminal adapter.

## Current limits

- This is a local IPC prototype, not an authenticated remote service. TCP exposure needs CURVE or another explicit authenticated transport policy.
- The replay journal is bounded by decoder safety ceilings but is not compacted into periodic native checkpoints.
- Any attached client may resize the authoritative PTY; the last serialized resize wins.
- The plain `muxctl` client replays bytes into the host terminal and cannot reconstruct historical resize rendering exactly.
- Ghostty effect callbacks that write responses to the child PTY are not yet bridged.
- Unix PTY/TTY support is implemented for Linux and macOS; Windows is not implemented.

## Reproduction

On Debian or Ubuntu:

```sh
sudo apt-get install libzmq5 build-essential
make check
make race
make build
./bin/muxd -session work
# in another terminal
./bin/muxctl -session work
```
