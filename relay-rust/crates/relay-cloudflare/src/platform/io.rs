use super::storage::{Database, MANIFEST, api};
use crate::{config, persistence, runtime::Host};
use anyhow::{Result, anyhow, ensure};
use js_sys::{ArrayBuffer, Uint8Array};
use wasm_bindgen::JsCast;
use windshare_relay_core::{
    challenge::ChallengeSeed,
    clock::UnixMillis,
    lifecycle::{ConnectionRef, Incarnation},
    share::{CheckpointTarget, Identity, Storage},
};
use worker::{State, WebSocket, worker_sys::ext::WebSocketExt};

const SOCKET_OPEN: u16 = 1;
const CLOSE_NORMAL: u16 = 1000;
const MAX_JS_DATE_MS: u64 = 8_640_000_000_000_000;

pub fn random<const N: usize>() -> Result<[u8; N]> {
    let mut bytes = [0; N];
    getrandom::fill(&mut bytes).map_err(|error| anyhow!("Worker entropy: {error}"))?;
    Ok(bytes)
}

pub fn incarnation() -> Result<Incarnation> {
    Incarnation::new(random()?).ok_or_else(|| anyhow!("zero incarnation"))
}

#[expect(
    clippy::cast_possible_truncation,
    clippy::cast_sign_loss,
    reason = "Workers Date.now is a nonnegative integer millisecond timestamp below 2^53"
)]
pub fn now() -> UnixMillis {
    UnixMillis(js_sys::Date::now() as u64)
}

#[expect(
    clippy::cast_precision_loss,
    reason = "JavaScript Date's supported integer range is below 2^53"
)]
pub fn alarm_time(at: u64) -> Result<worker::ScheduledTime> {
    ensure!(
        at <= MAX_JS_DATE_MS,
        "alarm timestamp exceeds JavaScript Date range"
    );
    // The SDK interprets an integer as a relative delay, but kernel deadlines
    // are absolute and must survive reconstruction without extending their life.
    Ok(worker::ScheduledTime::new(js_sys::Date::new(
        &wasm_bindgen::JsValue::from_f64(at as f64),
    )))
}

pub fn connection(state: &State, ws: &WebSocket) -> Result<ConnectionRef> {
    let connections: Vec<_> = state
        .get_tags(ws)
        .iter()
        .filter_map(|tag| persistence::parse_tag(tag))
        .collect();
    ensure!(connections.len() == 1, "invalid connection tags");
    Ok(connections[0])
}

pub fn socket(state: &State, connection: ConnectionRef) -> Option<WebSocket> {
    state
        .get_websockets_with_tag(&persistence::tag(connection))
        .into_iter()
        .find(is_open)
}

pub fn is_open(ws: &WebSocket) -> bool {
    ws.as_ref().ready_state() == SOCKET_OPEN
}

pub fn attachment(ws: &WebSocket) -> Result<Vec<u8>> {
    let value = ws
        .as_ref()
        .deserialize_attachment()
        .map_err(|error| anyhow!("{error:?}"))?;
    ensure!(
        value.is_instance_of::<Uint8Array>() || value.is_instance_of::<ArrayBuffer>(),
        "missing binary attachment"
    );
    let bytes = Uint8Array::new(&value);
    ensure!(
        bytes.length() as usize <= config::budget().payload_bytes(),
        "attachment bound"
    );
    Ok(bytes.to_vec())
}

pub struct CloudflareHost<'a> {
    pub state: &'a State,
    pub database: Database,
    pub identity: Identity,
}

impl Host for CloudflareHost<'_> {
    fn now(&self) -> UnixMillis {
        now()
    }

    fn challenge(&mut self) -> Result<ChallengeSeed> {
        ChallengeSeed::new(random()?, random()?).ok_or_else(|| anyhow!("zero challenge entropy"))
    }

    fn execute(&mut self, operation: &Storage) -> Result<()> {
        self.database.execute(self.identity, operation)
    }

    fn send(&mut self, connection: ConnectionRef, bytes: &[u8]) -> Result<()> {
        let ws = socket(self.state, connection).ok_or_else(|| anyhow!("socket unavailable"))?;
        api(ws.send_with_bytes(bytes))
    }

    fn close(&mut self, connection: ConnectionRef) -> Result<()> {
        if let Some(ws) = socket(self.state, connection) {
            api(ws.close(Some(CLOSE_NORMAL), Some("relay control ended")))?;
        }
        Ok(())
    }

    fn checkpoint(&mut self, target: CheckpointTarget, bytes: &[u8]) -> Result<()> {
        match target {
            CheckpointTarget::Manifest => self.database.put_bytes(MANIFEST, bytes),
            CheckpointTarget::Generation(connection) => {
                if let Some(ws) = socket(self.state, connection) {
                    ensure!(
                        bytes.len() <= config::budget().payload_bytes(),
                        "attachment budget"
                    );
                    // Use a typed byte array, avoiding serde's JS number-array expansion.
                    // serializeAttachment itself enforces the actual structured-clone limit.
                    ws.as_ref()
                        .serialize_attachment(Uint8Array::from(bytes).into())
                        .map_err(|error| anyhow!("attachment rejected: {error:?}"))?;
                }
                Ok(())
            }
        }
    }
}
