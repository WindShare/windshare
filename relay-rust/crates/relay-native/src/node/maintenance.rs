use super::{Configuration, Identity};
use crate::node::access::AuthorityState;
use anyhow::{Context, Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use rusqlite::Connection;
use std::time::Duration;
use std::{
    fs::{File, OpenOptions},
    io::{Read, Write},
    path::{Path, PathBuf},
};
use windshare_relay_access::usage::Admission;
use windshare_relay_core::{clock::UnixMillis, lifecycle::Record};

// Bound SQLite waits while holding the exclusive `.admission` gate lock so
// live traffic is not stalled before `network-upgrade` retries on exit 75.
const READ_BUSY_TIMEOUT: Duration = Duration::from_millis(250);

#[derive(Clone, Copy)]
pub enum Transition {
    Enter {
        policy_version: u64,
        reconnect_budget: Duration,
    },
    Release,
}

fn has_reconnect_window(db: &Connection, required_until: UnixMillis) -> Result<bool> {
    // The partial index contains only live routes and pending stops. Historical
    // STOPs can grow indefinitely and must never delay a maintenance fence.
    let mut statement = db.prepare(&format!(
        "SELECT record FROM shares WHERE {}",
        crate::storage::recovery_filter()
    ))?;
    let mut rows = statement.query([])?;
    while let Some(row) = rows.next()? {
        let bytes: Vec<u8> = row.get(0)?;
        let record =
            Record::decode(&bytes).map_err(|e| anyhow::anyhow!("invalid relay record: {e:?}"))?;
        if let Record::Grace(grace) = record
            && grace.deadline() < required_until
        {
            return Ok(false);
        }
    }
    Ok(true)
}

#[derive(Clone)]
pub(crate) struct Gate(PathBuf);
impl Gate {
    fn path(database: &Path) -> Result<PathBuf> {
        let mut path = database.canonicalize()?.into_os_string();
        path.push(".admission");
        Ok(path.into())
    }
    pub(crate) fn install(database: &Path) -> Result<Self> {
        let path = Self::path(database)?;
        OpenOptions::new()
            .create(true)
            .truncate(false)
            .write(true)
            .open(&path)?;
        Ok(Self(path))
    }
    // The receiver owns this shared file lock from JOIN inspection until its
    // socket leaves the actor, covering the pending and admitted session alike.
    pub(crate) fn admit(&self, probe: bool) -> Result<File> {
        let file = File::open(&self.0)?;
        file.try_lock_shared()?;
        ensure!(
            probe || file.metadata()?.len() == 0,
            "probe-only maintenance"
        );
        Ok(file)
    }
    pub(crate) fn open_to_users(&self) -> bool {
        self.0.metadata().is_ok_and(|m| m.len() == 0)
    }
}

fn check_operation(config: &Configuration, node_key: &str, operation: &str) -> Result<()> {
    ensure!(
        (16..=64).contains(&operation.len())
            && operation
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'-'),
        "invalid operation identifier"
    );
    check_identity(config, node_key)
}

/// # Errors
/// Rejects an absent identity or a configuration for a different enrolled node.
pub fn check_identity(config: &Configuration, node_key: &str) -> Result<()> {
    let identity: Identity = serde_json::from_slice(&std::fs::read(&config.identity_file)?)?;
    ensure!(
        URL_SAFE_NO_PAD.encode(identity.key().verifying_key().to_bytes()) == node_key,
        "wrong maintenance host"
    );
    Ok(())
}

/// # Errors
/// Rejects an absent or foreign hold before changing the running release.
pub fn check_hold(config: &Configuration, node_key: &str, operation: &str) -> Result<()> {
    check_operation(config, node_key, operation)?;
    let mut file = File::open(Gate::path(&config.database)?)?;
    file.try_lock_shared()?;
    let mut owner = String::new();
    (&mut file).take(65).read_to_string(&mut owner)?;
    ensure!(owner == operation, "operation does not own the local hold");
    Ok(())
}

/// Operates on the installed host's admission gate under its local OS credentials.
///
/// Returns `Ok(false)` when a live JOIN, tight reconnect window, or transient
/// SQLite lock defers the transition.
///
/// # Errors
/// Fails if credentials, gate ownership, database state, or the signed drain
/// policy are invalid.
pub fn change(
    config: &Configuration,
    node_key: &str,
    operation: &str,
    transition: Transition,
) -> Result<bool> {
    change_with_read_timeout(config, node_key, operation, transition, READ_BUSY_TIMEOUT)
}

fn change_with_read_timeout(
    config: &Configuration,
    node_key: &str,
    operation: &str,
    transition: Transition,
    read_busy_timeout: Duration,
) -> Result<bool> {
    check_operation(config, node_key, operation)?;
    let mut file = OpenOptions::new()
        .read(true)
        .write(true)
        .open(Gate::path(&config.database)?)
        .context("running relay has no admission gate")?;
    if matches!(transition, Transition::Release) {
        match file.try_lock_shared() {
            Ok(()) => {}
            Err(std::fs::TryLockError::WouldBlock) => return Ok(false),
            Err(e) => return Err(e.into()),
        }
        if file.metadata()?.len() == 0 {
            return Ok(true);
        }
        file.unlock()?;
    }
    match file.try_lock() {
        Ok(()) => {}
        Err(std::fs::TryLockError::WouldBlock) => return Ok(false),
        Err(e) => return Err(e.into()),
    }
    let mut owner = String::new();
    (&mut file).take(65).read_to_string(&mut owner)?;
    ensure!(
        owner.is_empty() || owner == operation,
        "another operation owns the local hold"
    );
    if let Transition::Enter {
        policy_version,
        reconnect_budget,
    } = transition
    {
        let issuer = config
            .node
            .authority
            .as_ref()
            .context("missing reporting authority")?
            .issuer()?;
        ensure!(
            !reconnect_budget.is_zero()
                && reconnect_budget <= windshare_relay_core::lifecycle::SENDER_GRACE,
            "invalid reconnect budget"
        );
        match check_enter_database(
            &config.database,
            issuer,
            policy_version,
            reconnect_budget,
            read_busy_timeout,
        ) {
            Ok(true) => {}
            Ok(false) => return Ok(false),
            Err(error) if is_database_busy(&error) => {
                tracing::info!(
                    event = "local_maintenance_deferred",
                    operation_id = %operation,
                    node_id = %node_key,
                    reason = "database_busy",
                    %error,
                );
                return Ok(false);
            }
            Err(error) => return Err(error),
        }
        if owner.is_empty() {
            file.write_all(operation.as_bytes())?;
        }
    } else if !owner.is_empty() {
        file.set_len(0)?;
    }
    file.sync_all()?;
    tracing::info!(event="local_maintenance_changed",operation_id=%operation,node_id=%node_key,probe_only=matches!(transition, Transition::Enter { .. }));
    Ok(true)
}

fn check_enter_database(
    database: &Path,
    issuer: [u8; 32],
    policy_version: u64,
    reconnect_budget: Duration,
    read_busy_timeout: Duration,
) -> Result<bool> {
    let db = crate::storage::open_read_only(database, read_busy_timeout)?;
    let body: Vec<u8> = db.query_row(
        "SELECT body FROM access_authorities WHERE issuer=?1",
        [issuer],
        |r| r.get(0),
    )?;
    let state: AuthorityState = serde_json::from_slice(&body)?;
    ensure!(
        state
            .policy
            .is_some_and(|p| p.version == policy_version && p.admission == Admission::Draining),
        "drain policy changed"
    );
    let now = crate::environment::Environment::now(&crate::environment::NativeEnvironment::new()?);
    has_reconnect_window(&db, now.saturating_add(reconnect_budget))
}

fn is_database_busy(error: &anyhow::Error) -> bool {
    error
        .chain()
        .filter_map(|cause| cause.downcast_ref::<rusqlite::Error>())
        .any(crate::storage::is_busy)
}

#[cfg(test)]
#[path = "maintenance_tests.rs"]
mod tests;
