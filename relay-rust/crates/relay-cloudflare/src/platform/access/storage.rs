use super::{failure, text_id};
use serde::{Serialize, de::DeserializeOwned};
use windshare_relay_access::{Credential, Digest, Enrollment};
use worker::{Result, SqlStorage, SqlStorageValue, State};

const TERMINATION_BATCH_SIZE: i64 = 32;
pub const TERMINATION_RETRY_MS: u64 = 5_000;

pub struct Database(SqlStorage);
impl Database {
    pub fn open(state: &State) -> Result<Self> {
        let db = Self(state.storage().sql());
        db.0.exec("CREATE TABLE IF NOT EXISTS access_entries (key TEXT PRIMARY KEY, value TEXT NOT NULL, expires INTEGER) STRICT", None)?;
        db.0.exec("CREATE TABLE IF NOT EXISTS access_credentials (digest TEXT PRIMARY KEY, id TEXT UNIQUE NOT NULL) STRICT", None)?;
        db.0.exec("CREATE TABLE IF NOT EXISTS credential_shares (credential TEXT NOT NULL, share TEXT NOT NULL, PRIMARY KEY(credential,share)) STRICT", None)?;
        db.0.exec("CREATE TABLE IF NOT EXISTS termination_queue (credential TEXT NOT NULL, share TEXT NOT NULL, next_attempt INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(credential,share)) STRICT", None)?;
        Ok(db)
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
        // Expiry reclaims credentials whose installation was never confirmed.
        for enrollment in self.list::<Enrollment>("enrollment:")? {
            if !matches!(
                enrollment.state,
                windshare_relay_access::EnrollmentState::Installed { .. }
            ) && enrollment.expires_at <= now
            {
                if let Some(id) = enrollment.incomplete_credential() {
                    self.remove_credential(id)?;
                }
                self.delete(&format!("request:{}", text_id(&enrollment.claim_digest.0)))?;
                self.delete(&format!("enrollment:{}", text_id(&enrollment.id)))?;
            }
        }
        self.0.exec(
            "DELETE FROM access_entries WHERE expires IS NOT NULL AND expires<=?",
            Some(vec![i64::try_from(now).map_err(failure)?.into()]),
        )?;
        Ok(())
    }
}
