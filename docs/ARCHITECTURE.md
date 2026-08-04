# Architecture

## Invariants

The implementation is organized around seven invariants:

1. **One authoritative PTY writer.** All client input reaches one ROUTER actor and then one reducer-backed runtime command. No client owns the PTY master.
2. **One ordered state stream.** PTY bytes, resizes, and exit each consume exactly one sequence number.
3. **Snapshot before tee, without a subscription race.** The SUB socket is ready before attach is acknowledged; the snapshot has sequence `S`; any concurrent publication is `> S`.
4. **Shen is the control-plane authority.** A typed command is reduced to either a new opaque state plus declarative effects or a typed rejection; Go executes effects but does not independently authorize them.
5. **Illegal host values are unconstructable outside `shenguard`.** Guard fields are private and transitions return new values.
6. **Endpoint ownership is exclusive.** Before stale IPC cleanup or bind, the daemon holds a nonblocking advisory lock on a persistent owner-only sentinel for each filesystem endpoint; a competing daemon cannot unlink a live socket.
7. **Only attached clients may write or resize.** The ROUTER identity becomes an opaque `ClientID`; the Shen reducer authorizes commands before Go touches PTY or terminal state.

## Planes

### Control plane

The daemon owns a ZeroMQ ROUTER socket. Every client owns a DEALER socket whose identity is its validated client ID. ROUTER serializes attach, input, resize, detach, ping, and resync requests. This gives the PTY a single input path without adding application-level locks around multiple socket readers.

Request IDs are used only where a response is required. High-frequency input and resize messages use request ID zero and receive no success acknowledgement; a server-side rejection is still returned as an asynchronous error. After decoding and identity checks, the ROUTER submits a typed command to the Shen reducer. The reducer owns membership, control ownership, exit state, writer-lock state, sequence allocation, and rejection reasons. It returns effects such as `write-pty`, `resize-pty`, `publish`, `capture-snapshot`, or `reply`; the Go runtime executes those effects under its transaction and actor rules.

### Filesystem endpoint ownership

Default IPC paths live beneath `/tmp/shenmux-<euid>/`, which is created or verified as a real directory owned by the effective UID with mode `0700`. Custom IPC endpoint parents must satisfy the same ownership and permission rule. For every control and data path, the daemon opens a sibling `.lock` sentinel with mode `0600` and takes `flock(LOCK_EX|LOCK_NB)` before stale-socket removal or bind. The sentinel remains on disk by design; the kernel releases ownership when the daemon exits, and a later daemon can safely reacquire it.

### Data plane

The daemon owns an XPUB socket and clients own SUB sockets. The sole session topic carries PTY, resize, and exit events. XPUB also sees subscription commands. A unique `ready/<client-id>` subscription acts as an attachment barrier.

ZeroMQ PUB/SUB is intentionally allowed to drop under pressure. Reliability comes from sequence detection plus targeted resynchronization, not from pretending the broadcast socket is a durable queue.

## Snapshot algorithm

1. Client creates SUB and subscribes to `session/<name>` and `ready/<id>`.
2. Client connects DEALER and sends `attach`.
3. ROUTER waits until XPUB has observed `ready/<id>`.
4. Runtime acquires its mutex and submits `begin-attach(client)` to the Shen reducer; this records the pending client and acquires the writer lock.
5. Under the same lock it clones the replay journal and terminal metadata at sequence `S`.
6. Runtime submits `finish-attach(client, snapshot-metadata)` with the frozen metadata, then releases the lock.
7. The archive is compressed outside the lock.
8. Runtime executes the reducer's targeted-reply effect; ROUTER returns `attached(S, archive)`.
9. Client replays the archive, discards already-buffered events `<= S`, and applies `S+1...`.

The runtime lock is held only while cloning state, not while gzip compressing it. PTY output emitted during compression is therefore not blocked and is already queued at the subscriber.

## Runtime transaction order

For each command or PTY event, the runtime:

1. constructs a typed command and submits it to the Shen reducer while holding the runtime state lock;
2. receives either a rejection or the next control-plane state plus declarative effects;
3. executes synchronous impure effects in their required order (including PTY/terminal rollback where resize can fail); asynchronous PTY writes are handed to the single writer actor;
4. appends the immutable journal event and commits the canonical screen/model together;
5. releases the mutex and hands publication to the XPUB actor.

A publication failure cannot roll back the PTY, journal, or model. Clients that missed the event detect a gap and resynchronize from the journal.

Resize is serialized with PTY output. Shen first validates authorization, dimensions, and the next sequence. Go then resizes the OS PTY and VT state adapter before committing the event; if the authoritative terminal rejects the resize, Go restores the PTY's prior size. Any failure after terminal reflow is a consistency failure that closes the runtime rather than publishing a partial transition. Input remains asynchronous through the single PTY writer actor and does not hold the runtime lock while a write blocks.

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
