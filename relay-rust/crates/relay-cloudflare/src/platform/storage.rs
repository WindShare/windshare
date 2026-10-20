use crate::persistence::{AuthorityRow, Image};
use anyhow::{Result, anyhow, ensure};
use serde::{Serialize, de::DeserializeOwned};
use windshare_relay_core::{
    authority::{self, StoredAuthority},
    share::{Identity, Storage},
};
use worker::{SqlStorage, SqlStorageValue, State};

pub const IMAGE: &str = "identity";
pub const MANIFEST: &str = "manifest";
const AUTHORITY: &str = "authority";

pub fn api<T>(result: worker::Result<T>) -> Result<T> {
    result.map_err(|error| anyhow!("{error}"))
}

#[derive(Clone)]
pub struct Database(SqlStorage);

impl Database {
    pub fn open(state: &State) -> Result<Self> {
        let database = Self(state.storage().sql());
        api(database.0.exec(
            "CREATE TABLE IF NOT EXISTS relay_lifecycle (key TEXT PRIMARY KEY, value BLOB NOT NULL) STRICT",
            None,
        ))?;
        Ok(database)
    }

    pub fn bytes(&self, key: &str) -> Result<Option<Vec<u8>>> {
        let cursor = api(self.0.exec(
            "SELECT value FROM relay_lifecycle WHERE key=?",
            Some(vec![key.into()]),
        ))?;
        let row = api(cursor.raw().next().transpose())?;
        row.map_or_else(
            || Ok(None),
            |row| match row.as_slice() {
                [SqlStorageValue::Blob(bytes)] => Ok(Some(bytes.clone())),
                _ => Err(anyhow!("invalid storage row")),
            },
        )
    }

    pub fn get<T: DeserializeOwned>(&self, key: &str) -> Result<Option<T>> {
        self.bytes(key)?
            .map(|bytes| serde_json::from_slice(&bytes).map_err(Into::into))
            .transpose()
    }

    pub fn put(&self, key: &str, value: &impl Serialize) -> Result<()> {
        self.put_bytes(key, &serde_json::to_vec(value)?)
    }

    pub fn put_bytes(&self, key: &str, bytes: &[u8]) -> Result<()> {
        api(self.0.exec(
            "INSERT INTO relay_lifecycle(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            Some(vec![key.into(), bytes.to_vec().into()]),
        ))?;
        Ok(())
    }

    pub fn new_image(&self, fresh: Option<&crate::config::Route>) -> Result<Image> {
        ensure!(
            self.bytes(AUTHORITY)?.is_none() && self.bytes(MANIFEST)?.is_none(),
            "identity missing from nonempty storage"
        );
        Ok(Image::new(
            fresh.ok_or_else(|| anyhow!("uninitialized object"))?,
        ))
    }

    pub fn uninitialized(&self) -> Result<bool> {
        Ok(self.bytes(IMAGE)?.is_none()
            && self.bytes(AUTHORITY)?.is_none()
            && self.bytes(MANIFEST)?.is_none())
    }

    pub fn authority(&self, identity: Identity) -> Result<Option<StoredAuthority>> {
        self.get::<AuthorityRow>(AUTHORITY)?
            .map(|row| row.decode(identity.relay, identity.share))
            .transpose()
    }

    pub fn execute(
        &self,
        identity: Identity,
        operation: &Storage,
    ) -> Result<Option<StoredAuthority>> {
        let current = self.authority(identity)?;
        let next = authority::apply(identity.relay, identity.share, current.as_ref(), operation)
            .map_err(|error| anyhow!("authority transition: {error:?}"))?;
        if current == next {
            return Ok(current);
        }
        // A complete row is replaced by one synchronous SQL statement. Automatic
        // transactions/output gates couple this write to all later sends in the event.
        if let Some(next) = &next {
            self.put(AUTHORITY, &AuthorityRow::new(next))?;
        } else {
            api(self.0.exec(
                "DELETE FROM relay_lifecycle WHERE key=?",
                Some(vec![AUTHORITY.into()]),
            ))?;
        }
        Ok(next)
    }
}
