# Session and workspace history

shenmux now persists the latest authoritative terminal archive when a runtime
is started with `--history-dir` (the default is `<state-dir>/history`). Each
screen delta, control transition, and process exit atomically replaces the
session record. The record contains the compressed protocol archive, so it can
be decoded without replaying raw PTY bytes.

This is session history, not workspace history. The intended command-center
model keeps the two related but separate:

```text
session
├── durable terminal event/checkpoint archive
├── workspace identity
├── git/worktree state
├── named workspace checkpoints
└── optional disposable overlay
```

Workspace checkpoints should use Git/worktrees for source and human-readable
diffs, plus content-addressed archives for non-Git files. OverlayFS or
container layers remain an implementation option for disposable sandboxes,
not the user-facing history model.

A future checkpoint manifest is expected to record:

```json
{
  "session_id": "sess_123",
  "workspace_id": "ws_456",
  "checkpoint_id": "cp_789",
  "parent": "cp_788",
  "git_commit": "abc123",
  "working_tree_patch": "sha256:...",
  "untracked_archive": "sha256:...",
  "reason": "before agent retry"
}
```

The lifecycle hooks are deliberately ordered: create a session checkpoint at
start, before delegated or destructive work, on explicit request, and on clean
completion. Terminal history answers “what happened?”; workspace checkpoints
answer “what can I safely restore or fork?”.
