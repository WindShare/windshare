use crate::{Store, challenges::REGISTRATION_EMAIL_PROOF_PREFIX};
use anyhow::{Result, anyhow};
use std::{cell::RefCell, rc::Rc};
use wasm_bindgen::{JsCast, JsValue, closure::Closure};
use worker::{SqlStorage, SqlStorageValue, State};

const EXPIRED_PROOF_BATCH_SIZE: i64 = 64;

#[derive(Clone)]
pub struct Database(SqlStorage);
fn api<T>(value: worker::Result<T>) -> Result<T> {
    value.map_err(|error| anyhow!("{error}"))
}
impl Database {
    pub fn open(state: &State) -> Result<Self> {
        let database = Self(state.storage().sql());
        api(database.0.exec("CREATE TABLE IF NOT EXISTS authority_records(key TEXT PRIMARY KEY,value TEXT NOT NULL) STRICT",None))?;
        Ok(database)
    }

    pub fn prune_registration_email_proofs(&self, now: u64) -> Result<()> {
        let registration_email = format!("key GLOB '{REGISTRATION_EMAIL_PROOF_PREFIX}/*'");
        let expiry = "json_extract(value, '$.expires_at')";
        // Only registration email traffic creates these rows. Reclaim a bounded
        // batch on successful delivery to keep abandoned attempts from accumulating.
        api(self.0.exec(
            &format!(
                "CREATE INDEX IF NOT EXISTS registration_email_expiry ON authority_records ({expiry}) WHERE {registration_email}"
            ),
            None,
        ))?;
        api(self.0.exec(
            &format!(
                "DELETE FROM authority_records WHERE key IN (
                    SELECT key FROM authority_records WHERE {registration_email} AND {expiry} <= ?
                    ORDER BY {expiry} LIMIT ?
                )"
            ),
            Some(vec![
                i64::try_from(now)?.into(),
                EXPIRED_PROOF_BATCH_SIZE.into(),
            ]),
        ))?;
        Ok(())
    }
}
impl Store for Database {
    fn get(&self, key: &str) -> Result<Option<String>> {
        let cursor = api(self.0.exec(
            "SELECT value FROM authority_records WHERE key=?",
            Some(vec![key.into()]),
        ))?;
        let row = api(cursor.raw().next().transpose())?;
        row.map(|row| match row.as_slice() {
            [SqlStorageValue::String(value)] => Ok(value.clone()),
            _ => Err(anyhow!("invalid record")),
        })
        .transpose()
    }
    fn put(&mut self, key: &str, value: &str) -> Result<()> {
        api(self.0.exec("INSERT INTO authority_records VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",Some(vec![key.into(),value.into()])))?;
        Ok(())
    }
    fn delete(&mut self, key: &str) -> Result<()> {
        api(self.0.exec(
            "DELETE FROM authority_records WHERE key=?",
            Some(vec![key.into()]),
        ))?;
        Ok(())
    }
    fn recent_reports(&self, limit: usize) -> Result<Vec<crate::Report>> {
        api(self.0.exec(
            "CREATE INDEX IF NOT EXISTS authority_reports_newest
             ON authority_records (json_extract(value, '$.created_at') DESC, key DESC)
             WHERE key GLOB 'report/*'",
            None,
        ))?;
        let rows = api(self.0.exec(
            "SELECT value FROM authority_records WHERE key GLOB 'report/*'
             ORDER BY json_extract(value, '$.created_at') DESC, key DESC LIMIT ?",
            Some(vec![i64::try_from(limit)?.into()]),
        ))?;
        rows.raw()
            .map(|row| match api(row)?.as_slice() {
                [SqlStorageValue::String(value)] => Ok(serde_json::from_str(value)?),
                _ => Err(anyhow!("invalid report row")),
            })
            .collect()
    }
    fn scan_after(
        &self,
        prefix: &str,
        after: Option<&str>,
        limit: Option<usize>,
    ) -> Result<Vec<(String, String)>> {
        let end = format!("{prefix}\u{10ffff}");
        let query = if after.is_some() {
            "SELECT key,value FROM authority_records WHERE key>? AND key<? ORDER BY key LIMIT ?"
        } else {
            "SELECT key,value FROM authority_records WHERE key>=? AND key<? ORDER BY key LIMIT ?"
        };
        let cursor = api(self.0.exec(
            query,
            Some(vec![
                after.unwrap_or(prefix).into(),
                end.into(),
                limit.map(i64::try_from).transpose()?.unwrap_or(-1).into(),
            ]),
        ))?;
        cursor
            .raw()
            .map(|row| match api(row)?.as_slice() {
                [SqlStorageValue::String(key), SqlStorageValue::String(value)] => {
                    Ok((key.clone(), value.clone()))
                }
                _ => Err(anyhow!("invalid records")),
            })
            .collect()
    }
}

/// The Rust Worker binding exposes the raw storage for APIs it has not wrapped.
pub fn transaction<T: 'static>(
    state: &State,
    action: impl FnOnce() -> Result<T> + 'static,
) -> Result<T> {
    let storage = state.storage();
    let raw: &JsValue = storage.as_raw().as_ref();
    let method = js_sys::Reflect::get(raw, &"transactionSync".into())
        .map_err(|error| anyhow!("{error:?}"))?
        .dyn_into::<js_sys::Function>()
        .map_err(|_| anyhow!("transactionSync unavailable"))?;
    let output = Rc::new(RefCell::new(None));
    let result = Rc::clone(&output);
    let callback = Closure::once(move || -> std::result::Result<JsValue, JsValue> {
        let value = action();
        let failed = value.is_err();
        *result.borrow_mut() = Some(value);
        if failed {
            Err(JsValue::from_str("authority transaction aborted"))
        } else {
            Ok(JsValue::UNDEFINED)
        }
    });
    let called = method.call1(raw, callback.as_ref());
    let result = output.borrow_mut().take();
    if let Some(result) = result {
        return result;
    }
    Err(anyhow!("authority transaction did not execute: {called:?}"))
}
