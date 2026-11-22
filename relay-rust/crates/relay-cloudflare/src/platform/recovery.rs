use super::{
    io::{self, CloudflareHost},
    storage::{Database, IMAGE, api},
    trace::Trace,
};
use crate::{
    config,
    persistence::Image,
    runtime::{Recovery, Runtime},
};
use anyhow::{Result, ensure};
use serde_json::json;
use windshare_relay_core::share::{ConnectionSnapshot, Identity};
use worker::State;

pub struct Ready {
    pub runtime: Runtime<Trace>,
    pub image: Image,
    pub identity: Identity,
    pub database: Database,
}

pub fn restore(state: &State, route: Option<&config::Route>) -> Result<Ready> {
    let database = Database::open(state)?;
    let existing = database.get::<Image>(IMAGE)?;
    let mut image = if let Some(image) = &existing {
        image.clone()
    } else {
        ensure!(
            state.get_websockets().is_empty(),
            "identity missing with surviving sockets"
        );
        database.new_image(route)?
    };
    let identity = image.identity(io::incarnation()?)?;
    let authority = database.authority(identity)?;
    let mut surviving = Vec::new();
    let mut records = Vec::new();
    for ws in state.get_websockets().iter().filter(|ws| io::is_open(ws)) {
        if let Ok(connection) = io::connection(state, ws) {
            surviving.push(connection);
            if let Ok(bytes) = io::attachment(ws)
                && let Ok(record) = ConnectionSnapshot::decode(&bytes, config::limits())
                && record.connection() == connection
            {
                records.push(record);
            }
        } else {
            api(ws.close(Some(1008), Some("invalid connection identity")))?;
        }
    }
    let observer = Trace {
        object: state.id().to_string(),
        identity,
    };
    observer.event(
        "host_restore",
        &json!({"surviving": surviving.len(), "snapshots": records.len()}),
    );
    let last_session = image.last_session;
    let mut host = CloudflareHost {
        state,
        database: database.clone(),
        identity,
        image: &mut image,
    };
    let runtime = Runtime::restore(
        &mut host,
        identity,
        if existing.is_some() {
            Recovery::Existing {
                authority: authority.as_ref(),
                last_session,
                surviving: &surviving,
                connections: &records,
            }
        } else {
            Recovery::New
        },
        observer,
    )?;
    if existing.is_none() {
        host.database.put(IMAGE, host.image)?;
    }
    Ok(Ready {
        runtime,
        image,
        identity,
        database,
    })
}
