use super::{
    io::{self, CloudflareHost},
    storage::{Database, IMAGE, MANIFEST, api},
    trace::Trace,
};
use crate::{config, persistence::Image, runtime::Runtime};
use anyhow::Result;
use serde_json::json;
use windshare_relay_core::share::{Checkpoint, CheckpointManifest, ConnectionCheckpoint, Identity};
use worker::State;

pub struct Ready {
    pub runtime: Runtime<Trace>,
    pub image: Image,
    pub identity: Identity,
}

pub fn restore(state: &State, route: Option<&config::Route>) -> Result<Ready> {
    let database = Database::open(state)?;
    let image = database.image(route)?;
    let identity = image.identity(io::incarnation()?)?;
    let record = database.authority(identity)?.map(|stored| stored.record());
    let manifest = database
        .bytes(MANIFEST)?
        .map(|bytes| CheckpointManifest::decode(&bytes, config::limits()))
        .transpose()
        .map_err(|error| anyhow::anyhow!("invalid manifest: {error:?}"))?;
    let mut surviving = Vec::new();
    let mut records = Vec::new();
    for ws in state.get_websockets().iter().filter(|ws| io::is_open(ws)) {
        if let Ok(connection) = io::connection(state, ws) {
            surviving.push(connection);
            if let Ok(bytes) = io::attachment(ws)
                && let Ok(record) = ConnectionCheckpoint::decode(&bytes, config::limits())
                && record.connection() == connection
            {
                records.push(record);
            }
        } else {
            api(ws.close(Some(1008), Some("invalid connection identity")))?;
        }
    }
    let checkpoint = manifest
        .as_ref()
        .map(|manifest| Checkpoint::from_connections(manifest, records, config::limits()))
        .transpose()
        .map_err(|error| anyhow::anyhow!("checkpoint refused: {error:?}"))?;
    let observer = Trace {
        object: state.id().to_string(),
        identity,
    };
    observer.event(
        "host_restore",
        &json!({"surviving": surviving.len(), "checkpoint": checkpoint.is_some()}),
    );
    let mut host = CloudflareHost {
        state,
        database,
        identity,
    };
    let runtime = Runtime::restore(
        &mut host,
        identity,
        record,
        checkpoint.as_ref(),
        &surviving,
        observer,
    )?;
    // The allocator is independent of attachment survival and never rolls back
    // when an individual connection or handshake fails.
    host.database.put(IMAGE, &image)?;
    Ok(Ready {
        runtime,
        image,
        identity,
    })
}
