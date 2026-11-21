use crate::{
    Store,
    fleet::publication::{self, Publication},
};
use anyhow::Result;
use std::future::Future;

const DELIVERED: &str = "directory/kv-digest";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ReconcileMode {
    /// Interactive mutations trust SQLite's delivery ledger so unchanged heads return without KV I/O.
    PendingOnly,
    /// Scheduled ticks verify KV still holds the current body in case the namespace was cleared or replaced.
    AuditKv,
}

pub trait DirectoryMirror {
    fn read(&self) -> impl Future<Output = Result<Option<String>>>;
    fn write(&self, body: &str) -> impl Future<Output = Result<()>>;
}

struct Snapshot {
    head: Publication,
    digest: String,
    body: String,
}

impl Snapshot {
    fn current(store: &impl Store) -> Result<Self> {
        let head = publication::current(store)?;
        let digest = publication::digest(&head.envelope)?;
        let body = serde_json::to_string(&head.envelope)?;
        Ok(Self { head, digest, body })
    }
}

/// Publishes the latest committed directory snapshot from SQLite to the public KV mirror.
///
/// Callers must serialize invocations so concurrent reconciliations cannot race on `DELIVERED`
/// or issue overlapping writes to the same KV key.
#[allow(
    clippy::future_not_send,
    reason = "Workers run JS futures on one isolate thread"
)]
pub async fn reconcile<S: Store, M: DirectoryMirror>(
    store: &mut S,
    mirror: &M,
    mode: ReconcileMode,
) -> Result<bool> {
    let mut snapshot = Snapshot::current(store)?;
    let mut reason = "pending";
    if store.get(DELIVERED)?.as_deref() == Some(snapshot.digest.as_str()) {
        match mode {
            ReconcileMode::PendingOnly => return Ok(false),
            ReconcileMode::AuditKv => {
                if mirror.read().await?.as_deref() == Some(snapshot.body.as_str()) {
                    return Ok(false);
                }
                // Concurrent transactions do not acquire the publishing lock, so SQLite may have
                // advanced while awaiting KV. Re-read so drift repair publishes the newest head
                // rather than regressing KV and burning the 1-write/sec per-key budget.
                let audited = snapshot.digest;
                snapshot = Snapshot::current(store)?;
                reason = if snapshot.digest == audited {
                    "audit_repair"
                } else {
                    "audit_superseded"
                };
            }
        }
    }
    mirror.write(&snapshot.body).await?;
    store.put(DELIVERED, &snapshot.digest)?;
    let revision = snapshot.head.revision()?;
    tracing::info!(
        event = "directory_kv_published",
        operation_id = %snapshot.digest,
        generation = revision.generation,
        serial = revision.serial,
        revision = %snapshot.digest,
        reason
    );
    Ok(true)
}
