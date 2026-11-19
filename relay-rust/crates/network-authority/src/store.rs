use anyhow::Result;
use serde::{Serialize, de::DeserializeOwned};

/// Each request's synchronous policy work runs in one host-owned transaction.
pub trait Store {
    /// # Errors
    /// Propagates storage read failures.
    fn get(&self, key: &str) -> Result<Option<String>>;
    /// # Errors
    /// Propagates storage write failures.
    fn put(&mut self, key: &str, value: &str) -> Result<()>;
    /// # Errors
    /// Propagates storage write failures.
    fn delete(&mut self, key: &str) -> Result<()>;
    /// # Errors
    /// Propagates storage read failures.
    fn scan(&self, prefix: &str, limit: Option<usize>) -> Result<Vec<(String, String)>> {
        self.scan_after(prefix, None, limit)
    }
    /// # Errors
    /// Propagates storage read failures.
    fn scan_after(
        &self,
        prefix: &str,
        after: Option<&str>,
        limit: Option<usize>,
    ) -> Result<Vec<(String, String)>>;
    /// # Errors
    /// Propagates storage read or report decoding failures.
    fn reports_after(
        &self,
        after: Option<crate::ReportPosition>,
        limit: usize,
    ) -> Result<Vec<crate::Report>>;
}
pub trait Environment {
    fn now(&self) -> u64;
    fn registration_email_configured(&self) -> bool;
    /// # Errors
    /// Fails if cryptographic entropy is unavailable.
    fn random<const N: usize>(&mut self) -> Result<[u8; N]>;
}
pub fn load<T: DeserializeOwned>(store: &impl Store, key: &str) -> Result<Option<T>> {
    store
        .get(key)?
        .map(|value| Ok(serde_json::from_str(&value)?))
        .transpose()
}
pub fn save(store: &mut impl Store, key: &str, value: &impl Serialize) -> Result<()> {
    store.put(key, &serde_json::to_string(value)?)
}
pub fn key(prefix: &str, id: &[u8]) -> String {
    use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
    format!("{prefix}/{}", URL_SAFE_NO_PAD.encode(id))
}
