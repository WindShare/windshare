use crate::{
    Store,
    fleet::publication::{self, Publication},
    store::{load, save},
};
use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::future::Future;

const DELIVERY_STATE: &str = "directory/kv-delivery";

/// Minimum spacing between KV writes to the single directory key, leaving margin
/// past Cloudflare KV's 1-write/sec per-key limit.
pub const WRITE_COOLDOWN_MS: u64 = 2_000;

/// Window after a KV write during which scheduled audits trust SQLite's delivery
/// ledger instead of reading KV's 60 s eventually-consistent edge cache.
pub const AUDIT_SETTLE_MS: u64 = 120_000;

/// Spread retries across a transient KV outage before the hourly audit takes over.
pub const RETRY_BACKOFF_MS: [u64; 6] = [2_000, 5_000, 15_000, 60_000, 300_000, 900_000];

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ReconcileMode {
    /// Interactive mutations and alarm retries trust SQLite's delivery ledger so
    /// unchanged heads return without KV I/O.
    PendingOnly,
    /// Scheduled ticks verify KV still holds the current body once the propagation
    /// window has elapsed, in case the namespace was cleared or replaced.
    AuditKv,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[must_use]
pub enum ReconcileOutcome {
    /// Published the latest committed directory snapshot to KV.
    Published,
    /// Delivery ledger matches the current head and is still within KV's propagation window.
    Settling,
    /// Delivery ledger matches the current head (and KV agreed when audited).
    UpToDate,
    /// SQLite has an unpublished head, deferred until KV's per-key write cooldown elapses.
    Cooldown { retry_after_ms: u64 },
    /// KV write failed; `retry_after_ms` is `Some` while bounded alarm retries remain,
    /// or `None` once rapid retries are exhausted and recovery falls back to scheduled ticks.
    RetryPending {
        retry_after_ms: Option<u64>,
        attempts: u8,
    },
}

impl ReconcileOutcome {
    pub const fn retry_after_ms(self) -> Option<u64> {
        match self {
            Self::Cooldown { retry_after_ms } => Some(retry_after_ms),
            Self::RetryPending { retry_after_ms, .. } => retry_after_ms,
            Self::Published | Self::Settling | Self::UpToDate => None,
        }
    }
}

pub trait DirectoryMirror {
    fn read(&self) -> impl Future<Output = Result<Option<String>>>;
    fn write(&self, body: &str) -> impl Future<Output = Result<()>>;
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
struct DeliveryRecord {
    digest: String,
    delivered_at_ms: u64,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
struct DeliveryState {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    delivered: Option<DeliveryRecord>,
    #[serde(default, skip_serializing_if = "is_zero")]
    failed_attempts: u8,
}

#[expect(
    clippy::trivially_copy_pass_by_ref,
    reason = "serde skip_serializing_if passes a reference to the field"
)]
const fn is_zero(value: &u8) -> bool {
    *value == 0
}

impl DeliveryState {
    fn load(store: &impl Store) -> Result<Self> {
        Ok(load(store, DELIVERY_STATE)?.unwrap_or_default())
    }

    fn save(&self, store: &mut impl Store) -> Result<()> {
        save(store, DELIVERY_STATE, self)
    }

    fn matches(&self, digest: &str) -> bool {
        self.delivered
            .as_ref()
            .is_some_and(|record| record.digest == digest)
    }

    fn is_settling(&self, now_ms: u64) -> bool {
        self.delivered.as_ref().is_some_and(|record| {
            now_ms
                .checked_sub(record.delivered_at_ms)
                .is_some_and(|elapsed| elapsed < AUDIT_SETTLE_MS)
        })
    }

    fn write_cooldown_ms(&self, now_ms: u64) -> Option<u64> {
        let record = self.delivered.as_ref()?;
        let elapsed = now_ms.checked_sub(record.delivered_at_ms)?;
        WRITE_COOLDOWN_MS
            .checked_sub(elapsed)
            .filter(|&remaining| remaining > 0)
    }
}

pub fn sync_pending(store: &impl Store, digest: &str) -> Result<bool> {
    let state = DeliveryState::load(store)?;
    Ok(!state.matches(digest) || state.failed_attempts > 0)
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

#[derive(Clone, Copy)]
struct WriteContext {
    mode: ReconcileMode,
    reason: &'static str,
    repair_audit: bool,
}

/// Publishes the latest committed directory snapshot from SQLite to the public KV mirror.
///
/// Callers must serialize invocations so concurrent reconciliations cannot race on
/// `DELIVERY_STATE` or issue overlapping writes to the same KV key.
#[allow(
    clippy::future_not_send,
    reason = "Workers run JS futures on one isolate thread"
)]
pub async fn reconcile<S: Store, M: DirectoryMirror>(
    store: &mut S,
    mirror: &M,
    mode: ReconcileMode,
    mut now_ms: impl FnMut() -> u64,
) -> Result<ReconcileOutcome> {
    let mut snapshot = Snapshot::current(store)?;
    let mut state = DeliveryState::load(store)?;
    let mut context = WriteContext {
        mode,
        reason: "pending",
        repair_audit: false,
    };

    if state.matches(&snapshot.digest) {
        if state.is_settling(now_ms()) {
            if mode == ReconcileMode::AuditKv {
                tracing::debug!(
                    event = "directory_kv_audit_skipped",
                    operation_id = %snapshot.digest,
                    reason = "settling"
                );
            }
            return Ok(ReconcileOutcome::Settling);
        }
        match mode {
            ReconcileMode::PendingOnly => return Ok(ReconcileOutcome::UpToDate),
            ReconcileMode::AuditKv => {
                if mirror.read().await?.as_deref() == Some(snapshot.body.as_str()) {
                    return Ok(ReconcileOutcome::UpToDate);
                }
                // Concurrent transactions do not acquire the publishing lock, so SQLite may have
                // advanced while awaiting KV. Re-read so drift repair publishes the newest head
                // rather than regressing KV and burning the 1-write/sec per-key budget.
                let audited = snapshot.digest;
                snapshot = Snapshot::current(store)?;
                context.repair_audit = true;
                context.reason = if snapshot.digest == audited {
                    "audit_repair"
                } else {
                    "audit_superseded"
                };
            }
        }
    } else if let Some(retry_after_ms) = state.write_cooldown_ms(now_ms()) {
        tracing::info!(
            event = "directory_kv_write_deferred",
            operation_id = %snapshot.digest,
            ?mode,
            retry_after_ms,
            reason = "write_cooldown"
        );
        return Ok(ReconcileOutcome::Cooldown { retry_after_ms });
    }

    publish_snapshot(store, mirror, &snapshot, &mut state, context, now_ms).await
}

#[allow(
    clippy::future_not_send,
    reason = "Workers run JS futures on one isolate thread"
)]
async fn publish_snapshot<S: Store, M: DirectoryMirror>(
    store: &mut S,
    mirror: &M,
    snapshot: &Snapshot,
    state: &mut DeliveryState,
    context: WriteContext,
    mut now_ms: impl FnMut() -> u64,
) -> Result<ReconcileOutcome> {
    if let Err(error) = mirror.write(&snapshot.body).await {
        return record_write_failure(store, snapshot, state, context, &error);
    }
    let delivered_at_ms = now_ms();
    *state = DeliveryState {
        delivered: Some(DeliveryRecord {
            digest: snapshot.digest.clone(),
            delivered_at_ms,
        }),
        failed_attempts: 0,
    };
    state.save(store)?;
    let revision = snapshot.head.revision()?;
    tracing::info!(
        event = "directory_kv_published",
        operation_id = %snapshot.digest,
        generation = revision.generation,
        serial = revision.serial,
        revision = %snapshot.digest,
        delivered_at_ms,
        reason = context.reason
    );
    Ok(ReconcileOutcome::Published)
}

fn record_write_failure<S: Store>(
    store: &mut S,
    snapshot: &Snapshot,
    state: &mut DeliveryState,
    context: WriteContext,
    error: &anyhow::Error,
) -> Result<ReconcileOutcome> {
    if context.repair_audit {
        // Drift was already observed in KV; clear the stale delivery record so
        // PendingOnly alarm retries actually write KV instead of short-circuiting.
        state.delivered = None;
    }
    let retry_after_ms = RETRY_BACKOFF_MS
        .get(usize::from(state.failed_attempts))
        .copied();
    state.failed_attempts = state.failed_attempts.saturating_add(1);
    state.save(store)?;
    if let Some(delay_ms) = retry_after_ms {
        tracing::warn!(
            event = "directory_kv_retry_pending",
            operation_id = %snapshot.digest,
            mode = ?context.mode,
            reason = context.reason,
            attempts = state.failed_attempts,
            retry_after_ms = delay_ms,
            %error
        );
    } else {
        tracing::warn!(
            event = "directory_kv_retry_exhausted",
            operation_id = %snapshot.digest,
            mode = ?context.mode,
            reason = context.reason,
            attempts = state.failed_attempts,
            %error
        );
    }
    Ok(ReconcileOutcome::RetryPending {
        retry_after_ms,
        attempts: state.failed_attempts,
    })
}
