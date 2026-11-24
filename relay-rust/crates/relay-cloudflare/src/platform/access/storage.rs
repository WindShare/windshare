use super::failure;
use serde::{Serialize, de::DeserializeOwned};
use windshare_relay_access::{Credential, Digest, Enrollment};
use windshare_relay_console::{credentials::RevokedCredentialPosition, text_id};
use worker::{Result, SqlStorage, SqlStorageValue, State};

const CREDENTIAL_ROWS: &str = "key GLOB 'credential:*'";
const ENROLLMENT_ROWS: &str = "key GLOB 'enrollment:*'";
const ACTIVE_CREDENTIALS: &str = "json_extract(value, '$.state') = 'Active'";
const PENDING_ENROLLMENTS: &str = "json_extract(value, '$.state') = 'Pending'";
const REVOKED_AT: &str = "json_extract(value, '$.state.Revoked.at')";
const ENTRY_EXPIRY_INDEX: &str = "access_entry_expiry";
const ACTIVE_CREDENTIALS_INDEX: &str = "access_active_credentials";
const PENDING_ENROLLMENTS_INDEX: &str = "access_pending_enrollments";
const REVOKED_CREDENTIALS_INDEX: &str = "access_revoked_credentials";
const TERMINATION_BATCH_SIZE: i64 = 32;
pub const TERMINATION_RETRY_MS: u64 = 5_000;

fn active_credential_predicate() -> String {
    format!("{CREDENTIAL_ROWS} AND {ACTIVE_CREDENTIALS}")
}

fn pending_enrollment_predicate() -> String {
    format!("{ENROLLMENT_ROWS} AND expires IS NOT NULL AND {PENDING_ENROLLMENTS}")
}

fn revoked_credential_predicate() -> String {
    format!("{CREDENTIAL_ROWS} AND {REVOKED_AT} IS NOT NULL")
}

pub struct Database(SqlStorage);
impl Database {
    pub fn new(state: &State) -> Self {
        Self(state.storage().sql())
    }
    pub fn initialized(&self) -> Result<bool> {
        Ok(self
            .0
            .exec(
                "SELECT 1 FROM sqlite_schema WHERE type='table' AND name='access_entries'",
                None,
            )?
            .raw()
            .next()
            .transpose()?
            .is_some())
    }
    pub fn ensure_schema(&self) -> Result<()> {
        self.0.exec("CREATE TABLE IF NOT EXISTS access_entries (key TEXT PRIMARY KEY, value TEXT NOT NULL, expires INTEGER) STRICT", None)?;
        // Provision all partial indexes upfront so online requests and read paths
        // never execute DDL or trigger full-table index builds.
        self.0.exec(
            &format!(
                "CREATE INDEX IF NOT EXISTS {ENTRY_EXPIRY_INDEX}
                 ON access_entries(expires) WHERE expires IS NOT NULL"
            ),
            None,
        )?;
        let active = active_credential_predicate();
        self.0.exec(
            &format!(
                "CREATE INDEX IF NOT EXISTS {ACTIVE_CREDENTIALS_INDEX}
                 ON access_entries(key) WHERE {active}"
            ),
            None,
        )?;
        let pending = pending_enrollment_predicate();
        self.0.exec(
            &format!(
                "CREATE INDEX IF NOT EXISTS {PENDING_ENROLLMENTS_INDEX}
                 ON access_entries(key) WHERE {pending}"
            ),
            None,
        )?;
        let revoked = revoked_credential_predicate();
        self.0.exec(
            &format!(
                "CREATE INDEX IF NOT EXISTS {REVOKED_CREDENTIALS_INDEX}
                 ON access_entries ({REVOKED_AT} DESC, key DESC) WHERE {revoked}"
            ),
            None,
        )?;
        self.0.exec("CREATE TABLE IF NOT EXISTS access_credentials (digest TEXT PRIMARY KEY, id TEXT UNIQUE NOT NULL) STRICT", None)?;
        self.0.exec("CREATE TABLE IF NOT EXISTS credential_shares (credential TEXT NOT NULL, share TEXT NOT NULL, PRIMARY KEY(credential,share)) STRICT", None)?;
        self.0.exec("CREATE TABLE IF NOT EXISTS termination_queue (credential TEXT NOT NULL, share TEXT NOT NULL, next_attempt INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(credential,share)) STRICT", None)?;
        Ok(())
    }
    pub fn get<T: DeserializeOwned>(&self, key: &str) -> Result<Option<T>> {
        let rows = self.0.exec(
            "SELECT value FROM access_entries WHERE key=?",
            Some(vec![key.into()]),
        )?;
        match rows.raw().next().transpose()?.as_deref() {
            None => Ok(None),
            Some([SqlStorageValue::String(value)]) => {
                serde_json::from_str(value).map(Some).map_err(failure)
            }
            _ => Err(failure("invalid access row")),
        }
    }
    pub fn put(&self, key: &str, value: &impl Serialize, expires: Option<u64>) -> Result<()> {
        let expiry = expires.map_or(SqlStorageValue::Null, |at| {
            SqlStorageValue::Integer(i64::try_from(at).unwrap_or(i64::MAX))
        });
        self.0.exec("INSERT INTO access_entries(key,value,expires) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,expires=excluded.expires",
            Some(vec![key.into(), serde_json::to_string(value)?.into(), expiry]))?;
        Ok(())
    }
    pub fn delete(&self, key: &str) -> Result<()> {
        self.0.exec(
            "DELETE FROM access_entries WHERE key=?",
            Some(vec![key.into()]),
        )?;
        Ok(())
    }
    pub fn list<T: DeserializeOwned>(&self, prefix: &str) -> Result<Vec<T>> {
        self.0
            .exec(
                "SELECT value FROM access_entries WHERE key LIKE ? ORDER BY key LIMIT 1001",
                Some(vec![format!("{prefix}%").into()]),
            )?
            .raw()
            .map(|row| match row?.as_slice() {
                [SqlStorageValue::String(value)] => serde_json::from_str(value).map_err(failure),
                _ => Err(failure("invalid access row")),
            })
            .collect()
    }
    pub fn active_credentials(&self) -> Result<usize> {
        let active = active_credential_predicate();
        let rows = self.0.exec(
            &format!(
                "SELECT count(*) FROM access_entries INDEXED BY {ACTIVE_CREDENTIALS_INDEX} WHERE {active}"
            ),
            None,
        )?;
        match rows.raw().next().transpose()?.as_deref() {
            Some([SqlStorageValue::Integer(count)]) => usize::try_from(*count).map_err(failure),
            _ => Err(failure("invalid credential count")),
        }
    }
    pub fn list_active_credentials(&self) -> Result<Vec<Credential>> {
        let active = active_credential_predicate();
        self.read_values(
            &format!(
                "SELECT value FROM access_entries INDEXED BY {ACTIVE_CREDENTIALS_INDEX}
                WHERE {active} ORDER BY key"
            ),
            vec![],
            "invalid credential row",
        )
    }
    pub fn list_pending_enrollments(&self) -> Result<Vec<Enrollment>> {
        let pending = pending_enrollment_predicate();
        self.read_values(
            &format!(
                "SELECT value FROM access_entries INDEXED BY {PENDING_ENROLLMENTS_INDEX}
                WHERE {pending} ORDER BY key"
            ),
            vec![],
            "invalid enrollment row",
        )
    }
    pub fn pending_enrollments(&self) -> Result<usize> {
        let rows = self.0.exec(
            &format!(
                "SELECT count(*) FROM access_entries WHERE {ENROLLMENT_ROWS} AND expires IS NOT NULL"
            ),
            None,
        )?;
        match rows.raw().next().transpose()?.as_deref() {
            Some([SqlStorageValue::Integer(count)]) => usize::try_from(*count).map_err(failure),
            _ => Err(failure("invalid enrollment count")),
        }
    }
    pub fn revoked_credentials(
        &self,
        after: Option<&RevokedCredentialPosition>,
        limit: usize,
    ) -> Result<Vec<Credential>> {
        let revoked = revoked_credential_predicate();
        let read = |filter: &str, order: &str, mut args: Vec<SqlStorageValue>, take: usize| {
            args.push(i64::try_from(take).map_err(failure)?.into());
            self.read_values(
                &format!(
                    "SELECT value FROM access_entries INDEXED BY {REVOKED_CREDENTIALS_INDEX}
                    WHERE {revoked} {filter} ORDER BY {order} LIMIT ?"
                ),
                args,
                "invalid credential row",
            )
        };
        let order = format!("{REVOKED_AT} DESC, key DESC");
        let Some(after) = after else {
            return read("", &order, vec![], limit);
        };
        // Separate ranges keep a page bounded even when many revocations share a timestamp.
        let time = i64::try_from(after.revoked_at()).map_err(failure)?;
        let mut credentials = read(
            &format!("AND {REVOKED_AT} = ? AND key < ?"),
            "key DESC",
            vec![
                time.into(),
                format!("credential:{}", text_id(&after.credential_id())).into(),
            ],
            limit,
        )?;
        if credentials.len() < limit {
            credentials.extend(read(
                &format!("AND {REVOKED_AT} < ?"),
                &order,
                vec![time.into()],
                limit - credentials.len(),
            )?);
        }
        Ok(credentials)
    }
    fn read_values<T: DeserializeOwned>(
        &self,
        query: &str,
        args: Vec<SqlStorageValue>,
        invalid_row: &str,
    ) -> Result<Vec<T>> {
        self.0
            .exec(query, Some(args))?
            .raw()
            .map(|row| match row?.as_slice() {
                [SqlStorageValue::String(value)] => serde_json::from_str(value).map_err(failure),
                _ => Err(failure(invalid_row)),
            })
            .collect()
    }
    pub fn credential(&self, digest: Digest) -> Result<Option<Credential>> {
        let rows = self.0.exec(
            "SELECT id FROM access_credentials WHERE digest=?",
            Some(vec![text_id(&digest.0).into()]),
        )?;
        match rows.raw().next().transpose()?.as_deref() {
            None => Ok(None),
            Some([SqlStorageValue::String(id)]) => self.get(&format!("credential:{id}")),
            _ => Err(failure("invalid credential index")),
        }
    }
    pub fn save_credential(&self, credential: &Credential) -> Result<()> {
        let id = text_id(&credential.id);
        self.put(&format!("credential:{id}"), credential, None)?;
        self.0.exec("INSERT INTO access_credentials(digest,id) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET digest=excluded.digest",
            Some(vec![text_id(&credential.digest.0).into(), id.into()]))?;
        Ok(())
    }
    pub fn remove_credential(&self, id: [u8; 16]) -> Result<()> {
        let id = text_id(&id);
        self.delete(&format!("credential:{id}"))?;
        self.0.exec(
            "DELETE FROM access_credentials WHERE id=?",
            Some(vec![id.into()]),
        )?;
        Ok(())
    }
    pub fn track_share(&self, credential: &str, share: &str) -> Result<()> {
        self.0.exec(
            "INSERT OR IGNORE INTO credential_shares(credential,share) VALUES(?,?)",
            Some(vec![credential.into(), share.into()]),
        )?;
        Ok(())
    }
    pub fn queue_termination(&self, credential: &str) -> Result<()> {
        self.0.exec("INSERT OR IGNORE INTO termination_queue(credential,share) SELECT credential,share FROM credential_shares WHERE credential=?", Some(vec![credential.into()]))?;
        Ok(())
    }
    pub fn terminations(&self, now: u64) -> Result<Vec<(String, String)>> {
        self.0
            .exec(
                "SELECT credential,share FROM termination_queue WHERE next_attempt<=? ORDER BY next_attempt,credential,share LIMIT ?",
                Some(vec![i64::try_from(now).map_err(failure)?.into(), TERMINATION_BATCH_SIZE.into()]),
            )?
            .raw()
            .map(|row| match row?.as_slice() {
                [
                    SqlStorageValue::String(credential),
                    SqlStorageValue::String(share),
                ] => Ok((credential.clone(), share.clone())),
                _ => Err(failure("invalid termination queue")),
            })
            .collect()
    }
    pub fn next_termination(&self) -> Result<Option<u64>> {
        let rows = self
            .0
            .exec("SELECT MIN(next_attempt) FROM termination_queue", None)?;
        match rows.raw().next().transpose()?.as_deref() {
            Some([SqlStorageValue::Null]) | None => Ok(None),
            Some([SqlStorageValue::Integer(at)]) => u64::try_from(*at).map(Some).map_err(failure),
            _ => Err(failure("invalid termination deadline")),
        }
    }
    pub fn attempting(&self, credential: &str, share: &str, now: u64) -> Result<()> {
        // Move the deadline before yielding: a failed delivery or interrupted
        // alarm must leave later tasks eligible, without discarding this one.
        let retry_at = i64::try_from(now.saturating_add(TERMINATION_RETRY_MS)).map_err(failure)?;
        self.0.exec(
            "UPDATE termination_queue SET next_attempt=? WHERE credential=? AND share=?",
            Some(vec![retry_at.into(), credential.into(), share.into()]),
        )?;
        Ok(())
    }
    pub fn terminated(&self, credential: &str, share: &str) -> Result<()> {
        self.0.exec(
            "DELETE FROM termination_queue WHERE credential=? AND share=?",
            Some(vec![credential.into(), share.into()]),
        )?;
        self.0.exec(
            "DELETE FROM credential_shares WHERE credential=? AND share=?",
            Some(vec![credential.into(), share.into()]),
        )?;
        Ok(())
    }
    pub fn cleanup(&self, now: u64) -> Result<()> {
        self.0.exec(
            "DELETE FROM access_entries WHERE expires IS NOT NULL AND expires<=?",
            Some(vec![i64::try_from(now).map_err(failure)?.into()]),
        )?;
        Ok(())
    }
}
