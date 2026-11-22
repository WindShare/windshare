use crate::{ProofKind, Store};
use anyhow::{Result, anyhow};
use std::{cell::RefCell, rc::Rc};
use wasm_bindgen::{JsCast, JsValue, closure::Closure};
use worker::{SqlStorage, SqlStorageValue, State};

const EXPIRED_PROOF_BATCH_SIZE: i64 = 64;
const PROOF_EXPIRY_EXPR: &str = "json_extract(value, '$.expires_at')";
const REPORT_CREATED_AT_EXPR: &str = "json_extract(value, '$.created_at')";
const REPORTS_NEWEST_INDEX: &str = "authority_reports_newest";

const fn proof_expiry_index(kind: ProofKind) -> &'static str {
    match kind {
        ProofKind::Work => "registration_work_expiry",
        ProofKind::Email => "registration_email_expiry",
    }
}

#[derive(Clone)]
pub struct Database(SqlStorage);
fn api<T>(value: worker::Result<T>) -> Result<T> {
    value.map_err(|error| anyhow!("{error}"))
}
impl Database {
    pub fn new(state: &State) -> Self {
        Self(state.storage().sql())
    }

    pub fn ensure_schema(&self) -> Result<()> {
        api(self.0.exec(
            "CREATE TABLE IF NOT EXISTS authority_records(key TEXT PRIMARY KEY,value TEXT NOT NULL) STRICT",
            None,
        ))?;
        // Provision partial indexes upfront outside transactions so online requests
        // never trigger full-table index builds or execute DDL on read paths.
        for kind in [ProofKind::Work, ProofKind::Email] {
            let index = proof_expiry_index(kind);
            let proof_keys = format!("key GLOB '{}/*'", kind.registration_prefix());
            api(self.0.exec(
                &format!(
                    "CREATE INDEX IF NOT EXISTS {index} ON authority_records ({PROOF_EXPIRY_EXPR}) WHERE {proof_keys}"
                ),
                None,
            ))?;
        }
        api(self.0.exec(
            &format!(
                "CREATE INDEX IF NOT EXISTS {REPORTS_NEWEST_INDEX} \
                 ON authority_records ({REPORT_CREATED_AT_EXPR} DESC, key DESC) WHERE key GLOB 'report/*'"
            ),
            None,
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
    fn prune_registration_proofs(&mut self, kind: ProofKind, now: u64) -> Result<()> {
        let index = proof_expiry_index(kind);
        let proof_keys = format!("key GLOB '{}/*'", kind.registration_prefix());
        // Reclaim expired proof rows as new ones arrive, with bounded work per issuance.
        api(self.0.exec(
            &format!(
                "DELETE FROM authority_records WHERE key IN (
                    SELECT key FROM authority_records INDEXED BY {index}
                    WHERE {proof_keys} AND {PROOF_EXPIRY_EXPR} <= ?
                    ORDER BY {PROOF_EXPIRY_EXPR} LIMIT ?
                )"
            ),
            Some(vec![
                i64::try_from(now)?.into(),
                EXPIRED_PROOF_BATCH_SIZE.into(),
            ]),
        ))?;
        Ok(())
    }
    fn reports_after(
        &self,
        after: Option<crate::ReportPosition>,
        limit: usize,
    ) -> Result<Vec<crate::Report>> {
        let read = |filter: &str,
                    order: &str,
                    mut args: Vec<SqlStorageValue>,
                    limit: usize|
         -> Result<Vec<crate::Report>> {
            args.push(i64::try_from(limit)?.into());
            let rows = api(self.0.exec(
                &format!(
                    "SELECT value FROM authority_records INDEXED BY {REPORTS_NEWEST_INDEX}
                 WHERE key GLOB 'report/*' {filter} ORDER BY {order} LIMIT ?"
                ),
                Some(args),
            ))?;
            rows.raw()
                .map(|row| match api(row)?.as_slice() {
                    [SqlStorageValue::String(value)] => Ok(serde_json::from_str(value)?),
                    _ => Err(anyhow!("invalid report row")),
                })
                .collect()
        };
        let order = format!("{REPORT_CREATED_AT_EXPR} DESC, key DESC");
        let Some(after) = after else {
            return read("", &order, vec![], limit);
        };
        // Two index ranges let SQLite seek past both timestamp and ID ties.
        let time = i64::try_from(after.created_at)?;
        let mut reports = read(
            &format!("AND {REPORT_CREATED_AT_EXPR} = ? AND key < ?"),
            "key DESC",
            vec![time.into(), crate::store::key("report", &after.id).into()],
            limit,
        )?;
        if reports.len() < limit {
            reports.extend(read(
                &format!("AND {REPORT_CREATED_AT_EXPR} < ?"),
                &order,
                vec![time.into()],
                limit - reports.len(),
            )?);
        }
        Ok(reports)
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
