use super::{failure, fixture};
use js_sys::{ArrayBuffer, Uint8Array};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use wasm_bindgen::JsCast;
use windshare_relay_core::{
    lifecycle::{ConnectionRef, Record},
    share::{Checkpoint, CheckpointManifest, ConnectionCheckpoint},
};
use worker::worker_sys::ext::WebSocketExt;
use worker::{Result, SqlStorageValue, State, WebSocket};

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Mode {
    Attachments,
    SqliteMirror,
}

#[derive(Clone, Serialize, Deserialize)]
pub struct Image {
    pub mode: Mode,
    pub lifecycle: Option<Vec<u8>>,
    pub manifest: Vec<u8>,
    pub records: BTreeMap<String, Vec<u8>>,
    pub writes: u64,
    pub last_generation: [u64; 2],
    pub pending: bool,
}

impl Image {
    pub fn empty(mode: Mode) -> Self {
        Self {
            mode,
            lifecycle: None,
            manifest: vec![],
            records: BTreeMap::new(),
            writes: 0,
            last_generation: [0; 2],
            pending: false,
        }
    }

    pub fn record(&self) -> Result<Option<Record>> {
        self.lifecycle
            .as_deref()
            .map(Record::decode)
            .transpose()
            .map_err(failure)
    }

    pub fn recover(&self, state: &State) -> Result<Checkpoint> {
        if self.pending {
            return Err(failure("incomplete checkpoint event"));
        }
        let manifest =
            CheckpointManifest::decode(&self.manifest, fixture::limits()).map_err(failure)?;
        let mut records = Vec::new();
        for &connection in manifest.generations() {
            let bytes = if self.mode == Mode::SqliteMirror {
                self.records.get(&tag(connection)).cloned()
            } else {
                match socket(state, connection) {
                    Some(ws) => read_attachment(&ws)?,
                    None => self.records.get(&tag(connection)).cloned(),
                }
            }
            .ok_or_else(|| failure(format!("MissingResponsibility: {}", tag(connection))))?;
            records.push(ConnectionCheckpoint::decode(&bytes, fixture::limits()).map_err(failure)?);
        }
        Checkpoint::from_connections(&manifest, records, fixture::limits()).map_err(failure)
    }
}

pub fn load(state: &State) -> Result<Option<Image>> {
    let sql = state.storage().sql();
    sql.exec("CREATE TABLE IF NOT EXISTS checkpoint_image (id INTEGER PRIMARY KEY CHECK(id=1), value BLOB NOT NULL)", None)?;
    let row = sql
        .exec("SELECT value FROM checkpoint_image WHERE id=1", None)?
        .raw()
        .next()
        .transpose()?;
    match row {
        None => Ok(None),
        Some(row) => match &row[0] {
            SqlStorageValue::Blob(bytes) => Ok(Some(serde_json::from_slice(bytes)?)),
            _ => Err(failure("checkpoint image is not a blob")),
        },
    }
}

pub fn save(state: &State, image: &mut Image) -> Result<()> {
    image.writes += 1;
    // One row publishes manifest and records atomically; a half-updated inventory
    // must never make an earlier generation appear settled.
    state.storage().sql().exec(
        "INSERT INTO checkpoint_image(id,value) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET value=excluded.value",
        Some(vec![serde_json::to_vec(image)?.into()]),
    )?;
    Ok(())
}

pub fn tag(connection: ConnectionRef) -> String {
    format!("c-{}-{}", connection.id, connection.generation)
}

pub fn socket(state: &State, connection: ConnectionRef) -> Option<WebSocket> {
    state
        .get_websockets_with_tag(&tag(connection))
        .into_iter()
        .find(|ws| ws.as_ref().ready_state() == 1)
}

pub fn write_attachment(ws: &WebSocket, bytes: &[u8]) -> Result<()> {
    if bytes.len() > fixture::budget().payload_bytes() {
        return Err(failure("attachment budget exceeded"));
    }
    // Calling the raw binding avoids serde's JavaScript number-array expansion.
    ws.as_ref()
        .serialize_attachment(Uint8Array::from(bytes).into())?;
    let roundtrip = read_attachment(ws)?.ok_or_else(|| failure("attachment disappeared"))?;
    if roundtrip != bytes {
        return Err(failure("attachment roundtrip mismatch"));
    }
    Ok(())
}

pub fn read_attachment(ws: &WebSocket) -> Result<Option<Vec<u8>>> {
    let value = ws.as_ref().deserialize_attachment()?;
    if value.is_null() || value.is_undefined() {
        return Ok(None);
    }
    if !value.is_instance_of::<Uint8Array>() && !value.is_instance_of::<ArrayBuffer>() {
        return Err(failure("attachment is not binary"));
    }
    Ok(Some(Uint8Array::new(&value).to_vec()))
}
