use anyhow::{Context, Result, anyhow, ensure};
use rusqlite::{Connection, OptionalExtension, TransactionBehavior, params};
use std::{
    fs::{File, OpenOptions},
    num::NonZeroU64,
    path::Path,
    sync::{Arc, Mutex},
    time::Duration,
};
use windshare_relay_core::{
    identifiers::{RelayIdentity, ShareId},
    lifecycle::Record,
    share::Storage,
};

mod transactions;
use transactions::read;

const SCHEMA_VERSION: i64 = 1;
const BUSY_TIMEOUT: Duration = Duration::from_secs(1);

pub trait Persistence: Clone + Send + Sync + 'static {
    fn load(&self, share: ShareId) -> Result<Option<Record>>;
    fn execute(&self, share: ShareId, operation: &Storage) -> Result<()>;
}

struct State {
    connection: Mutex<Connection>,
    // The lock is held until every outstanding blocking transaction has finished.
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
        let mut connection = Connection::open(path)?;
        connection.busy_timeout(BUSY_TIMEOUT)?;
        connection.execute_batch("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;")?;
        let generation = initialize(&mut connection, relay)?;
        Ok(Self(Arc::new(State {
            connection: Mutex::new(connection),
            _lease: lease,
            relay,
            generation,
        })))
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
        let mut statement = connection.prepare("SELECT share FROM shares")?;
        let shares = statement.query_map([], |row| row.get::<_, Vec<u8>>(0))?;
        let mut records = Vec::new();
        // Permanent STOP history is not an in-memory actor inventory.
        for bytes in shares {
            let share = ShareId::new(
                bytes?
                    .try_into()
                    .map_err(|_| anyhow!("invalid share key"))?,
            )
            .ok_or_else(|| anyhow!("zero share key"))?;
            let stored = read(&connection, self.0.relay, share)?
                .ok_or_else(|| anyhow!("authority disappeared"))?;
            if !matches!(stored.record(), Record::Stopped(_)) {
                ensure!(records.len() < limit, "recovering share capacity exhausted");
                records.push(stored.record());
            }
        }
        drop(statement);
        drop(connection);
        Ok(records)
    }
}

impl Persistence for Database {
    fn load(&self, share: ShareId) -> Result<Option<Record>> {
        let connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock poisoned"))?;
        let stored = read(&connection, self.0.relay, share)?;
        drop(connection);
        Ok(stored.map(|stored| stored.record()))
    }

    fn execute(&self, share: ShareId, operation: &Storage) -> Result<()> {
        let mut connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock poisoned"))?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        transactions::apply(&transaction, self.0.relay, share, operation)?;
        // Complete is evidence of a committed transaction, never of an attempted write.
        transaction.commit()?;
        drop(connection);
        Ok(())
    }
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
         CREATE TABLE IF NOT EXISTS shares (
            share BLOB PRIMARY KEY CHECK(length(share) = 12),
            record BLOB NOT NULL,
            descriptor BLOB,
            publication BLOB,
            CHECK((descriptor IS NULL) = (publication IS NULL))
         ) STRICT;",
    )?;
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
