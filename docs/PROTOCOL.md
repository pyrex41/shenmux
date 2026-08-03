# Protocol

## Multipart framing

Control messages have two or three application frames:

```text
[kind] [JSON metadata] [optional binary payload]
```

ROUTER adds the routing identity as the first transport frame. Published messages add the topic as the first application frame:

```text
[session/<name>] [kind] [JSON metadata] [optional binary payload]
```

Protocol version is currently `1`. Metadata is limited to 64 KiB and payloads to 256 MiB at the decoder boundary.

## Metadata

The JSON envelope may contain:

- `v`: protocol version;
- `session` and `client_id`;
- `request_id` for correlated control calls;
- `seq`;
- `cols`, `rows`;
- `cursor_x`, `cursor_y`, `alt_screen`;
- `exit_code`;
- `error`.

The ROUTER identity, not `client_id`, authorizes a request. When `client_id` is present it must match.

## Control kinds

| Kind | Direction | Payload | Result |
|---|---|---|---|
| `attach` | client → server | none | targeted `attached` snapshot |
| `resync` | client → server | none | targeted `attached` snapshot |
| `input` | client → server | raw bytes | no success reply unless requested |
| `resize` | client → server | dimensions in metadata | no success reply unless requested |
| `detach` | client → server | none | `pong` when correlated |
| `ping` | client → server | none | `pong` with current sequence/dimensions |
| `error` | server → client | error text in metadata | rejection |

## Data kinds

| Kind | Sequence-bearing | Payload |
|---|---:|---|
| `pty` | yes | raw PTY bytes |
| `resize` | yes | dimensions in metadata |
| `exit` | yes | exit code in metadata |

`attached` carries the snapshot archive and its terminal metadata. Every data event after a snapshot at `S` must be `S+1`, then `S+2`, and so on. Duplicate or old frames are discarded. A forward gap causes `resync`.

## Limits

- Client ID: 1–128 UTF-8 bytes, no NUL.
- Session name: 1–64 ASCII alphanumeric, dot, underscore, or hyphen.
- Dimensions: 1–65535 each.
- Input request: at most 1 MiB.
- PTY event in an archive: at most 16 MiB.
- Snapshot compressed payload: at most 256 MiB.
- Snapshot uncompressed representation: at most 512 MiB.
- Snapshot event count: at most 10 million.
