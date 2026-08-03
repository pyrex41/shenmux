# Architecture

## Invariants

The implementation is organized around six invariants:

1. **One authoritative PTY writer.** All client input reaches one ROUTER actor and then one guarded runtime method. No client owns the PTY master.
2. **One ordered state stream.** PTY bytes, resizes, and exit each consume exactly one sequence number.
3. **Snapshot before tee, without a subscription race.** The SUB socket is ready before attach is acknowledged; the snapshot has sequence `S`; any concurrent publication is `> S`.
4. **Only attached clients may write or resize.** The ROUTER identity becomes an opaque `ClientID`; the runtime calls `AcceptInput` before effects.
5. **Illegal host values are unconstructable outside `shenguard`.** Guard fields are private and transitions return new values.
6. **Endpoint ownership is exclusive.** Before stale IPC cleanup or bind, the daemon holds a nonblocking advisory lock on a persistent owner-only sentinel for each filesystem endpoint; a competing daemon cannot unlink a live socket.

## Planes

### Control plane

The daemon owns a ZeroMQ ROUTER socket. Every client owns a DEALER socket whose identity is its validated client ID. ROUTER serializes attach, input, resize, detach, ping, and resync requests. This gives the PTY a single input path without adding application-level locks around multiple socket readers.

Request IDs are used only where a response is required. High-frequency input and resize messages use request ID zero and receive no success acknowledgement; a server-side rejection is still returned as an asynchronous error.

### Filesystem endpoint ownership

Default IPC paths live beneath `/tmp/shenmux-<euid>/`, which is created or verified as a real directory owned by the effective UID with mode `0700`. Custom IPC endpoint parents must satisfy the same ownership and permission rule. For every control and data path, the daemon opens a sibling `.lock` sentinel with mode `0600` and takes `flock(LOCK_EX|LOCK_NB)` before stale-socket removal or bind. The sentinel remains on disk by design; the kernel releases ownership when the daemon exits, and a later daemon can safely reacquire it.

### Data plane

The daemon owns an XPUB socket and clients own SUB sockets. The sole session topic carries PTY, resize, and exit events. XPUB also sees subscription commands. A unique `ready/<client-id>` subscription acts as an attachment barrier.

ZeroMQ PUB/SUB is intentionally allowed to drop under pressure. Reliability comes from sequence detection plus targeted resynchronization, not from pretending the broadcast socket is a durable queue.

## Snapshot algorithm

1. Client creates SUB and subscribes to `session/<name>` and `ready/<id>`.
2. Client connects DEALER and sends `attach`.
3. ROUTER waits until XPUB has observed `ready/<id>`.
4. Runtime acquires its mutex and calls `BeginSnapshot`.
5. Under the same lock it clones the replay journal and terminal metadata at sequence `S`.
6. Runtime calls `EndSnapshot` and releases the lock.
7. The archive is compressed outside the lock.
8. Runtime marks the client attached and ROUTER returns `attached(S, archive)`.
9. Client replays the archive, discards already-buffered events `<= S`, and applies `S+1...`.

The runtime lock is held only while cloning state, not while gzip compressing it. PTY output emitted during compression is therefore not blocked and is already queued at the subscriber.

## Runtime transaction order

For each PTY event, the runtime:

1. feeds the VT state adapter;
2. computes the next opaque sequence;
3. appends an immutable journal event;
4. applies the guarded session transition;
5. releases the mutex;
6. asks the XPUB actor to publish.

A publication failure cannot roll back the PTY, journal, or model. Clients that missed the event detect a gap and resynchronize from the journal.

Resize is serialized with PTY output. The OS PTY and VT state adapter are resized before the sequence is committed. A failure at either impure edge aborts the event and is treated as a daemon error.

## Actor ownership

ZeroMQ sockets are not thread-safe. Each socket is confined to one goroutine:

- XPUB actor: subscription reception and publication sends.
- ROUTER actor: control receive and targeted replies.
- Client SUB actor: broadcast receive.
- Client DEALER actor: request send and response receive.

The Cgo wrapper deliberately does not put a broad I/O mutex around sockets; doing so would disguise invalid ownership rather than enforce a sound topology.

## Replay snapshot format

`SMXSNAP1` is gzip over a compact big-endian record stream:

- format version;
- initial columns and rows;
- event count;
- for each event: kind, flags, sequence, dimensions, exit code, payload length, payload.

The decoder enforces compressed and uncompressed limits, event count, per-event payload size, valid kinds, exact sequence continuity, and no trailing bytes.

## Replacing replay with native snapshots

A future Ghostty serializer can be introduced by versioning the snapshot payload and implementing a codec behind the existing envelope. The attach algorithm, sequence rules, and ZeroMQ topology do not change. A server may still retain a short post-checkpoint journal so events after the native snapshot can be replayed deterministically.
