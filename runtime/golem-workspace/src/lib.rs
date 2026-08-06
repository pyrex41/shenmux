use golem_rust::{agent_definition, agent_implementation, Schema};
use serde::{Deserialize, Serialize};

/// Durable metadata owned by the Golem agent. File bytes remain in the
/// browser/remote object protocol; this state is the small coordination seam
/// for snapshots, retries, and a future scheduled sync worker.
#[derive(Clone, Debug, Default, Schema, Serialize, Deserialize)]
pub struct WorkspaceMetadata {
    pub session_id: String,
    pub cwd: String,
    pub last_command: String,
    pub command_count: u64,
    pub sync_requested: bool,
    pub sync_sequence: u64,
    pub last_sync_sequence: u64,
}

#[agent_definition(mode = durable)]
pub trait WorkspaceAgent {
    fn new(session_id: String) -> Self;
    fn run(&mut self, cwd: String, command: String) -> String;
    fn metadata(&self) -> WorkspaceMetadata;
    /// Queue a durable background sync request. A scheduler/worker invokes
    /// `flush_sync` separately so a slow object store never blocks typing.
    fn request_sync(&mut self) -> u64;
    fn flush_sync(&mut self) -> u64;
}

pub struct WorkspaceAgentImpl {
    metadata: WorkspaceMetadata,
}

#[agent_implementation]
impl WorkspaceAgent for WorkspaceAgentImpl {
    fn new(session_id: String) -> Self {
        Self {
            metadata: WorkspaceMetadata {
                session_id,
                cwd: "/".into(),
                ..WorkspaceMetadata::default()
            },
        }
    }

    fn run(&mut self, cwd: String, command: String) -> String {
        self.metadata.cwd = cwd;
        self.metadata.last_command = command.clone();
        self.metadata.command_count += 1;
        if command.trim() == "sync" {
            self.request_sync();
            "sync queued".into()
        } else {
            format!("queued workspace command: {command}")
        }
    }

    fn metadata(&self) -> WorkspaceMetadata {
        self.metadata.clone()
    }

    fn request_sync(&mut self) -> u64 {
        self.metadata.sync_sequence += 1;
        self.metadata.sync_requested = true;
        self.metadata.sync_sequence
    }

    fn flush_sync(&mut self) -> u64 {
        self.metadata.last_sync_sequence = self.metadata.sync_sequence;
        self.metadata.sync_requested = false;
        self.metadata.last_sync_sequence
    }
}
