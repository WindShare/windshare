//! Experimental admission risk, never a receipt for delivered or physically released bytes.
//! Durable rows isolate the candidate policy from reconstruction and client-side timers.
use js_sys::Date;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use worker::{Result, State};

const WINDOW_FRAMES: i64 = 128;
const RESERVATIONS: i64 = 4;
const COOLDOWN_MS: i64 = 30_000;
const EVENT_LIMIT: i64 = 512;

#[derive(Clone, Copy, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Policy {
    Strict,
    Immediate,
    Cooldown,
}

#[derive(Deserialize, Serialize)]
struct Configuration {
    policy: String,
    retention_ms: Option<i64>,
}

#[derive(Deserialize, Serialize)]
struct Usage {
    generations: i64,
    charged_generations: i64,
    charged_frames: i64,
    expired_generations: i64,
    next_expiry_ms: Option<i64>,
}

pub fn prepare(state: &State) -> Result<()> {
    let sql = state.storage().sql();
    sql.exec("CREATE TABLE IF NOT EXISTS risk_policy (id INTEGER PRIMARY KEY CHECK(id=1), policy TEXT NOT NULL, retention_ms INTEGER)", None)?;
    sql.exec("CREATE TABLE IF NOT EXISTS risk_generation (role TEXT PRIMARY KEY, frames INTEGER NOT NULL, retired_ms INTEGER, expires_ms INTEGER)", None)?;
    sql.exec("CREATE TABLE IF NOT EXISTS probe_event (id INTEGER PRIMARY KEY AUTOINCREMENT, at_ms INTEGER NOT NULL, event TEXT NOT NULL, detail TEXT NOT NULL)", None)?;
    Ok(())
}

fn configuration(state: &State) -> Result<Option<Configuration>> {
    Ok(state
        .storage()
        .sql()
        .exec(
            "SELECT policy,retention_ms FROM risk_policy WHERE id=1",
            None,
        )?
        .to_array::<Configuration>()?
        .pop())
}

pub fn configure(state: &State, policy: Policy, connections: u64) -> Result<bool> {
    if connections != 0 || configuration(state)?.is_some() {
        return Ok(false);
    }
    let (name, retention) = match policy {
        Policy::Strict => ("strict", None),
        Policy::Immediate => ("immediate", Some(0)),
        Policy::Cooldown => ("cooldown", Some(COOLDOWN_MS)),
    };
    state.storage().sql().exec(
        "INSERT INTO risk_policy VALUES(1,?,?)",
        Some(vec![
            name.into(),
            retention.map_or(worker::SqlStorageValue::Null, Into::into),
        ]),
    )?;
    Ok(true)
}

fn usage(state: &State) -> Result<Usage> {
    state.storage().sql().exec(
        "SELECT COUNT(*) AS generations, COALESCE(SUM(CASE WHEN retired_ms IS NULL OR expires_ms IS NULL OR expires_ms>? THEN 1 ELSE 0 END),0) AS charged_generations, COALESCE(SUM(CASE WHEN retired_ms IS NULL OR expires_ms IS NULL OR expires_ms>? THEN frames ELSE 0 END),0) AS charged_frames, COALESCE(SUM(CASE WHEN retired_ms IS NOT NULL AND expires_ms<=? THEN 1 ELSE 0 END),0) AS expired_generations, MIN(CASE WHEN expires_ms>? THEN expires_ms END) AS next_expiry_ms FROM risk_generation",
        Some(vec![(Date::now() as i64).into(); 4]))?.one()
}

pub fn admit(state: &State, role: &str) -> Result<bool> {
    if configuration(state)?.is_none() || !role.starts_with("slow-") {
        return Ok(true);
    }
    let usage = usage(state)?;
    if usage.charged_generations >= RESERVATIONS
        || usage.charged_frames + WINDOW_FRAMES > RESERVATIONS * WINDOW_FRAMES
    {
        return Ok(false);
    }
    state.storage().sql().exec(
        "INSERT INTO risk_generation VALUES(?,?,NULL,NULL)",
        Some(vec![role.into(), WINDOW_FRAMES.into()]),
    )?;
    Ok(true)
}

pub fn permits_burst(state: &State, role: &str, frames: u64) -> Result<bool> {
    if configuration(state)?.is_none() {
        return Ok(true);
    }
    #[derive(Deserialize)]
    struct Count {
        permitted: i64,
    }
    let count: Count = state.storage().sql().exec(
        "SELECT COUNT(*) AS permitted FROM risk_generation WHERE role=? AND retired_ms IS NULL AND frames>=?",
        Some(vec![role.into(), (frames as i64).into()]))?.one()?;
    Ok(count.permitted == 1)
}

pub fn retire(state: &State, role: &str) -> Result<()> {
    let Some(config) = configuration(state)? else {
        return Err(worker::Error::RustError(
            "risk policy not configured".into(),
        ));
    };
    let now = Date::now() as i64;
    let expires = config.retention_ms.map(|hold| now + hold);
    // Retrying retirement cannot renew the deadline or restore the old sending authority.
    state.storage().sql().exec(
        "UPDATE risk_generation SET retired_ms=?,expires_ms=? WHERE role=? AND retired_ms IS NULL",
        Some(vec![
            now.into(),
            expires.map_or(worker::SqlStorageValue::Null, Into::into),
            role.into(),
        ]),
    )?;
    Ok(())
}

pub fn status(state: &State) -> Result<Value> {
    Ok(
        json!({"policy":configuration(state)?, "usage":usage(state)?,
        "window_frames":WINDOW_FRAMES, "reservation_limit":RESERVATIONS,
        "physical_release_proven":false, "production_kernel_policy":false}),
    )
}

pub fn record(state: &State, event: &str, detail: &Value) -> Result<()> {
    let sql = state.storage().sql();
    sql.exec(
        "INSERT INTO probe_event(at_ms,event,detail) VALUES(?,?,?)",
        Some(vec![
            (Date::now() as i64).into(),
            event.into(),
            detail.to_string().into(),
        ]),
    )?;
    sql.exec(
        "DELETE FROM probe_event WHERE id <= (SELECT COALESCE(MAX(id),0)-? FROM probe_event)",
        Some(vec![EVENT_LIMIT.into()]),
    )?;
    Ok(())
}

pub fn events(state: &State) -> Result<Value> {
    #[derive(Deserialize, Serialize)]
    struct Event {
        id: i64,
        at_ms: i64,
        event: String,
        detail: String,
    }
    Ok(json!(
        state
            .storage()
            .sql()
            .exec(
                "SELECT id,at_ms,event,detail FROM probe_event ORDER BY id",
                None
            )?
            .to_array::<Event>()?
    ))
}
