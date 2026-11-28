use anyhow::{Context, Result, anyhow, ensure};
use rusqlite::{Connection, OptionalExtension, TransactionBehavior, params};
use std::{
    collections::BTreeMap,
    fs::{File, OpenOptions},
    num::NonZeroU64,
    path::Path,
    sync::{Arc, Mutex},
    time::Duration,
};
use windshare_relay_core::{
    access::Attribution,
    authority::StoredAuthority,
    identifiers::{RelayIdentity, ShareId},
    lifecycle::Record,
    share::Storage,
};

mod access_control;
pub mod budget;
mod contribution;
mod discovery;
pub mod node_control;
mod transactions;
use transactions::read;

const SCHEMA_VERSION: i64 = 5;
const BUSY_TIMEOUT: Duration = Duration::from_secs(1);

pub trait Persistence: Clone + Send + Sync + 'static {
    fn load(&self, share: ShareId) -> Result<Option<StoredAuthority>>;
    fn execute(&self, share: ShareId, operation: &Storage) -> Result<Option<StoredAuthority>>;
}

struct State {
    connection: Mutex<Connection>,
    residents: Mutex<BTreeMap<ShareId, Attribution>>,
    path: std::path::PathBuf,
    // Generation-owned clones retain this lock through transport teardown and
    // blocking transactions, even when their actor or server has disappeared.
    _lease: File,
    relay: RelayIdentity,
    generation: NonZeroU64,
}

/// A single-process SQLite authority store; clones share the same ownership lease.
#[derive(Clone)]
pub struct Database(Arc<State>);

impl Database {
    /// Opens durable lifecycle storage and reserves a never-reused process generation.
    ///
    /// # Errors
    /// Refuses a second host, mismatched relay identity, unknown schema or corrupt authority.
    pub fn open(path: &Path, relay: RelayIdentity) -> Result<Self> {
        ensure!(
            path != Path::new(":memory:"),
            "native authority requires a durable database"
        );
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(path)?;
        let path = path.canonicalize()?;
        let mut lease_path = path.as_os_str().to_os_string();
        lease_path.push(".lock");
        let lease = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(lease_path)?;
        lease
            .try_lock()
            .context("another native host owns this database")?;
        drop(file);
        let mut connection = Connection::open(&path)?;
        connection.busy_timeout(BUSY_TIMEOUT)?;
        connection.execute_batch("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;")?;
        let generation = initialize(&mut connection, relay)?;
        Ok(Self(Arc::new(State {
            connection: Mutex::new(connection),
            residents: Mutex::default(),
            path,
            _lease: lease,
            relay,
            generation,
        })))
    }

    // Argon2 and credential writes must never hold the share ledger's SQLite writer lock.
    pub(crate) fn console_connection(&self) -> Result<Connection> {
        let mut path = self.0.path.as_os_str().to_os_string();
        path.push(".console.sqlite");
        let db = Connection::open(path)?;
        db.execute_batch("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;")?;
        db.busy_timeout(BUSY_TIMEOUT)?;
        Ok(db)
    }

    pub(crate) fn resident_inventory(&self) -> BTreeMap<ShareId, Attribution> {
        self.0.residents.lock().expect("resident inventory").clone()
    }

    pub(crate) fn generation(&self) -> NonZeroU64 {
        self.0.generation
    }

    pub(crate) fn recoverable_records(&self, limit: usize) -> Result<Vec<Record>> {
        let connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock poisoned"))?;
        let mut statement = connection.prepare(&format!(
            "SELECT share,record,descriptor FROM shares WHERE {}",
            recovery_filter()
        ))?;
        let mut rows = statement.query([])?;
        let mut records = Vec::new();
        // Permanent STOP history is not an in-memory actor inventory.
        while let Some(row) = rows.next()? {
            let share = ShareId::new(
                row.get::<_, Vec<u8>>(0)?
                    .try_into()
                    .map_err(|_| anyhow!("invalid share key"))?,
            )
            .ok_or_else(|| anyhow!("zero share key"))?;
            let bytes = row.get::<_, Vec<u8>>(1)?;
            let stored = StoredAuthority::decode(self.0.relay, share, &bytes, row.get(2)?)
                .map_err(|error| anyhow!("invalid durable authority: {error:?}"))?;
            if !matches!(stored.record(), Record::Stopped(_)) {
                ensure!(records.len() < limit, "recovering share capacity exhausted");
                records.push(stored.record());
            }
        }
        *self.0.residents.lock().expect("resident inventory") = records
            .iter()
            .filter_map(|record| {
                record
                    .live_attribution()
                    .map(|attribution| (record.share(), attribution))
            })
            .collect();
        drop(rows);
        drop(statement);
        drop(connection);
        Ok(records)
    }
}

impl Persistence for Database {
    fn load(&self, share: ShareId) -> Result<Option<StoredAuthority>> {
        let connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock poisoned"))?;
        let stored = read(&connection, self.0.relay, share)?;
        drop(connection);
        Ok(stored)
    }

    fn execute(&self, share: ShareId, operation: &Storage) -> Result<Option<StoredAuthority>> {
        let mut connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock poisoned"))?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let stored = transactions::apply(&transaction, self.0.relay, share, operation)?;
        // Complete is evidence of a committed transaction, never of an attempted write.
        transaction.commit()?;
        // Report committed idle and recovering shares even while an actor is being rebuilt.
        {
            let mut residents = self.0.residents.lock().expect("resident inventory");
            if let Some(attribution) = stored.as_ref().and_then(|s| s.record().live_attribution()) {
                residents.insert(share, attribution);
            } else {
                residents.remove(&share);
            }
        }
        drop(connection);
        Ok(stored)
    }
}

// Idle senders have a bounded reconnect window. A restart must scale with live
// routes and pending STOP cleanup, independently of the permanent STOP history.
// Unknown formats stay in the recovery set and still pass through full decoding.
pub fn recovery_filter() -> String {
    let ([format, kind], length) = Record::STOPPED_ENCODING;
    format!(
        "descriptor IS NOT NULL OR length(record) != {length} OR substr(record,1,2) != x'{format:02X}{kind:02X}'"
    )
}

fn initialize(connection: &mut Connection, relay: RelayIdentity) -> Result<NonZeroU64> {
    let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
    transaction.execute_batch(
        "CREATE TABLE IF NOT EXISTS host (
            singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
            format INTEGER NOT NULL,
            relay BLOB NOT NULL CHECK(length(relay) = 32),
            generation INTEGER NOT NULL CHECK(generation > 0)
         ) STRICT;
         CREATE TABLE IF NOT EXISTS discovery_cache(singleton INTEGER PRIMARY KEY CHECK(singleton=1),records BLOB NOT NULL) STRICT;
         CREATE TABLE IF NOT EXISTS contribution(singleton INTEGER PRIMARY KEY CHECK(singleton=1),policy TEXT NOT NULL) STRICT;
         CREATE TABLE IF NOT EXISTS shares (
            share BLOB PRIMARY KEY CHECK(length(share) = 12),
            record BLOB NOT NULL,
            descriptor BLOB
         ) STRICT;",
    )?;
    transaction.execute_batch(&format!(
        "CREATE INDEX IF NOT EXISTS shares_needing_recovery ON shares(share) WHERE {}",
        recovery_filter()
    ))?;
    let existing: Option<(i64, Vec<u8>, i64)> = transaction
        .query_row(
            "SELECT format, relay, generation FROM host WHERE singleton = 1",
            [],
            |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
        )
        .optional()?;
    let generation = if let Some((format, stored_relay, previous)) = existing {
        ensure!(
            format == SCHEMA_VERSION,
            "unsupported native authority schema"
        );
        ensure!(
            stored_relay == relay.as_bytes(),
            "database belongs to a different public endpoint"
        );
        previous
            .checked_add(1)
            .ok_or_else(|| anyhow!("connection generation exhausted"))?
    } else {
        let count: i64 =
            transaction.query_row("SELECT count(*) FROM shares", [], |row| row.get(0))?;
        ensure!(count == 0, "missing host identity for existing shares");
        1
    };
    transaction.execute(
        "INSERT INTO host VALUES (1, ?1, ?2, ?3)
         ON CONFLICT(singleton) DO UPDATE SET generation=excluded.generation",
        params![SCHEMA_VERSION, relay.as_bytes(), generation],
    )?;
    transaction.commit()?;
    NonZeroU64::new(u64::try_from(generation)?).ok_or_else(|| anyhow!("invalid generation"))
}

#[cfg(test)]
#[path = "storage_tests.rs"]
mod tests;
