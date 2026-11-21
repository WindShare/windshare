use super::{Configuration, Identity};
use crate::node::access::AuthorityState;
use anyhow::{Context, Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use rusqlite::Connection;
use std::time::Duration;
use std::{
    fmt,
    fs::{File, OpenOptions, TryLockError},
    io::{Read, Seek, Write},
    path::{Path, PathBuf},
    sync::{Arc, Mutex, MutexGuard, PoisonError},
};
use windshare_relay_access::usage::Admission;
use windshare_relay_core::{clock::UnixMillis, lifecycle::Record};

// Bound SQLite waits during read-only prechecks so `network-upgrade` retries
// promptly on exit 75 instead of stalling until its SSH attempt timeout.
const READ_BUSY_TIMEOUT: Duration = Duration::from_millis(250);
// Cap audit reads so a corrupted or oversized gate file cannot cause unbounded memory use.
const MAX_OWNER_PREVIEW_BYTES: u64 = 128;

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

#[derive(Debug)]
pub(crate) enum GateError {
    Maintenance,
    Io(std::io::Error),
}

impl fmt::Display for GateError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Maintenance => formatter.write_str("probe-only maintenance"),
            Self::Io(error) => write!(formatter, "maintenance gate I/O error: {error}"),
        }
    }
}

impl std::error::Error for GateError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Maintenance => None,
            Self::Io(error) => Some(error),
        }
    }
}

struct GateState {
    file: File,
    leases: usize,
    probe_only: bool,
    #[cfg(test)]
    fault: Option<std::io::ErrorKind>,
}

impl GateState {
    fn lock_shared(&self) -> Result<bool, GateError> {
        #[cfg(test)]
        if let Some(kind) = self.fault {
            return Err(GateError::Io(std::io::Error::from(kind)));
        }
        match self.file.try_lock_shared() {
            Ok(()) => {}
            Err(TryLockError::WouldBlock) => return Err(GateError::Maintenance),
            Err(TryLockError::Error(error)) => return Err(GateError::Io(error)),
        }
        match self.file.metadata() {
            Ok(metadata) => Ok(metadata.len() != 0),
            Err(error) => {
                self.unlock("admit_metadata_cleanup");
                Err(GateError::Io(error))
            }
        }
    }

    fn unlock(&self, operation: &'static str) {
        if let Err(error) = self.file.unlock() {
            tracing::warn!(
                event = "maintenance_gate_io_error",
                operation,
                %error,
            );
        }
    }

    fn is_probe_only(&self) -> Result<bool, std::io::Error> {
        #[cfg(test)]
        if let Some(kind) = self.fault {
            return Err(std::io::Error::from(kind));
        }
        self.file.metadata().map(|metadata| metadata.len() != 0)
    }
}

/// Reference-counted lease onto the process-wide shared maintenance lock.
pub(crate) struct JoinLease {
    state: Arc<Mutex<GateState>>,
}

impl Drop for JoinLease {
    fn drop(&mut self) {
        let mut state = self.state.lock().unwrap_or_else(PoisonError::into_inner);
        state.leases -= 1;
        if state.leases == 0 {
            state.probe_only = false;
            state.unlock("release_join_lease");
        }
    }
}

#[derive(Clone)]
pub(crate) struct Gate {
    state: Arc<Mutex<GateState>>,
}

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
        // Keep one read-only descriptor for the process lifetime so active joins
        // never multiply OS FDs or fail open/close under descriptor pressure.
        let mut file = File::open(&path)?;
        warn_if_fence_active(&mut file, &path);
        Ok(Self {
            state: Arc::new(Mutex::new(GateState {
                file,
                leases: 0,
                probe_only: false,
                #[cfg(test)]
                fault: None,
            })),
        })
    }

    fn lock_state(&self) -> MutexGuard<'_, GateState> {
        self.state.lock().unwrap_or_else(PoisonError::into_inner)
    }

    // Non-JOIN handshakes only need an admission check without retaining a lease.
    // While any JOIN lease holds the shared OS lock, no writer can mutate the fence,
    // so cached `probe_only` state stays valid without touching the filesystem.
    pub(crate) fn check(&self, probe: bool) -> Result<(), GateError> {
        let state = self.lock_state();
        if state.leases > 0 {
            let allowed = probe || !state.probe_only;
            drop(state);
            return if allowed {
                Ok(())
            } else {
                Err(GateError::Maintenance)
            };
        }
        let probe_only = state.lock_shared()?;
        state.unlock("check");
        drop(state);
        if !probe && probe_only {
            return Err(GateError::Maintenance);
        }
        Ok(())
    }

    // Receivers share one process-wide OS shared lock from JOIN inspection until
    // their socket leaves the actor, covering pending and admitted sessions alike.
    pub(crate) fn admit_join(&self, probe: bool) -> Result<JoinLease, GateError> {
        let mut state = self.lock_state();
        if state.leases > 0 {
            if !probe && state.probe_only {
                drop(state);
                return Err(GateError::Maintenance);
            }
            state.leases += 1;
            drop(state);
            return Ok(JoinLease {
                state: Arc::clone(&self.state),
            });
        }
        let probe_only = state.lock_shared()?;
        if !probe && probe_only {
            state.unlock("admit_join_refused");
            drop(state);
            return Err(GateError::Maintenance);
        }
        state.leases = 1;
        state.probe_only = probe_only;
        drop(state);
        Ok(JoinLease {
            state: Arc::clone(&self.state),
        })
    }

    pub(crate) fn open_to_users(&self) -> bool {
        let state = self.lock_state();
        if state.leases > 0 {
            return !state.probe_only;
        }
        let result = state.is_probe_only();
        drop(state);
        match result {
            Ok(probe_only) => !probe_only,
            Err(error) => {
                tracing::warn!(
                    event = "maintenance_gate_io_error",
                    operation = "open_to_users",
                    %error,
                );
                false
            }
        }
    }

    #[cfg(test)]
    pub(crate) fn set_io_fault(&self, kind: Option<std::io::ErrorKind>) {
        self.lock_state().fault = kind;
    }
}

fn read_owner_preview(file: &mut File) -> std::io::Result<String> {
    let mut raw = Vec::new();
    (&mut *file)
        .take(MAX_OWNER_PREVIEW_BYTES)
        .read_to_end(&mut raw)?;
    Ok(String::from_utf8_lossy(&raw).into_owned())
}

fn warn_if_fence_active(file: &mut File, path: &Path) {
    let Ok(metadata) = file.metadata() else {
        return;
    };
    let bytes = metadata.len();
    if bytes == 0 {
        return;
    }
    let owner = read_owner_preview(file).unwrap_or_else(|_| "<unreadable>".to_string());
    let _ = file.rewind();
    tracing::warn!(
        event = "local_maintenance_fence_active",
        gate = %path.display(),
        owner = %owner,
        bytes,
    );
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

/// Break-glass release of the installed host's admission gate regardless of recorded
/// owner, including corrupted or non-UTF-8 contents.
///
/// Returns `Ok(false)` when a live JOIN or concurrent gate transition holds the OS lock.
///
/// # Errors
/// Fails if `node_key` does not match the installed identity or the gate cannot be accessed.
pub fn force_release(config: &Configuration, node_key: &str) -> Result<bool> {
    check_identity(config, node_key)?;
    let mut file = OpenOptions::new()
        .read(true)
        .write(true)
        .open(Gate::path(&config.database)?)
        .context("running relay has no admission gate")?;
    match file.try_lock_shared() {
        Ok(()) => {}
        Err(TryLockError::WouldBlock) => return Ok(false),
        Err(error) => return Err(error.into()),
    }
    if file.metadata()?.len() == 0 {
        return Ok(true);
    }
    file.unlock()?;
    match file.try_lock() {
        Ok(()) => {}
        Err(TryLockError::WouldBlock) => return Ok(false),
        Err(error) => return Err(error.into()),
    }
    let previous_bytes = file.metadata()?.len();
    if previous_bytes == 0 {
        return Ok(true);
    }
    let previous_owner = read_owner_preview(&mut file)?;
    file.set_len(0)?;
    file.sync_all()?;
    tracing::warn!(
        event = "local_maintenance_force_released",
        node_id = %node_key,
        previous_owner = %previous_owner,
        previous_bytes,
    );
    Ok(true)
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
    match transition {
        Transition::Enter {
            policy_version,
            reconnect_budget,
        } => {
            // Precheck drain policy and grace windows outside the exclusive gate
            // lock so SQLite waits or deferred entries never refuse concurrent
            // JOIN or RESUME handshakes.
            if !preflight_enter(
                config,
                node_key,
                operation,
                policy_version,
                reconnect_budget,
                read_busy_timeout,
            )? {
                return Ok(false);
            }
        }
        Transition::Release => {
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
    match transition {
        Transition::Enter { .. } if owner.is_empty() => {
            file.write_all(operation.as_bytes())?;
        }
        Transition::Release if !owner.is_empty() => {
            file.set_len(0)?;
        }
        Transition::Enter { .. } | Transition::Release => {}
    }
    file.sync_all()?;
    tracing::info!(
        event = "local_maintenance_changed",
        operation_id = %operation,
        node_id = %node_key,
        probe_only = matches!(transition, Transition::Enter { .. }),
    );
    Ok(true)
}

fn preflight_enter(
    config: &Configuration,
    node_key: &str,
    operation: &str,
    policy_version: u64,
    reconnect_budget: Duration,
    read_busy_timeout: Duration,
) -> Result<bool> {
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
        Ok(ready) => Ok(ready),
        Err(error) if is_database_busy(&error) => {
            tracing::info!(
                event = "local_maintenance_deferred",
                operation_id = %operation,
                node_id = %node_key,
                reason = "database_busy",
                %error,
            );
            Ok(false)
        }
        Err(error) => Err(error),
    }
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
