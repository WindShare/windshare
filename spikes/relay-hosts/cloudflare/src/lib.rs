//! An authenticated, bounded experiment for host closure evidence, not a relay.
//! Nothing here interprets a WebSocket event as the kernel's physical-release receipt.
use js_sys::{Date, Reflect};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{cell::Cell, time::Duration};
use wasm_bindgen::JsCast;
use worker::*;

const MAX_CONNECTIONS: usize = 3;
const MAX_FRAMES: usize = 128;
const FRAME_BYTES: usize = 65_536;
const CLOSE_DEADLINE: Duration = Duration::from_secs(5);
const EVENT_LIMIT: i64 = 64;

#[event(fetch)]
async fn main(req: Request, env: Env, _ctx: Context) -> Result<Response> {
    let expected = format!("Bearer {}", env.secret("PROBE_TOKEN")?);
    if req.headers().get("Authorization")?.as_deref() != Some(expected.as_str()) {
        return Response::error("unauthorized", 401);
    }
    let url = req.url()?;
    let segments: Vec<_> = url
        .path()
        .split('/')
        .filter(|part| !part.is_empty())
        .collect();
    if segments.len() != 3
        || segments[0] != "probe"
        || segments[1].len() > 64
        || !segments[1]
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-')
    {
        return Response::error("expected /probe/<run>/<action>", 400);
    }
    env.durable_object("CLOSE_PROBE")?
        .get_by_name(segments[1])?
        .fetch_with_request(req)
        .await
}

#[derive(Serialize, Deserialize)]
struct Attachment {
    role: String,
    connected_at: f64,
}

#[derive(Deserialize)]
struct Burst {
    role: String,
    frames: usize,
    close: bool,
}

#[durable_object]
pub struct CloseProbe {
    state: State,
    initialized: Cell<bool>,
    boot: f64,
}

impl CloseProbe {
    fn initialize(&self) -> Result<()> {
        if !self.initialized.get() {
            self.state.storage().sql().exec(
                "CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY, detail TEXT NOT NULL)",
                None,
            )?;
            self.initialized.set(true);
        }
        Ok(())
    }

    fn record(&self, event: &str, details: Value) -> Result<()> {
        self.initialize()?;
        let value = json!({"event": event, "at_ms": Date::now(), "boot": self.boot,
            "object": self.state.id().to_string(), "details": details,
            "physical_release_proven": false});
        console_log!("{}", value);
        let sql = self.state.storage().sql();
        sql.exec(
            "INSERT INTO events(detail) VALUES (?)",
            Some(vec![value.to_string().into()]),
        )?;
        sql.exec(
            "DELETE FROM events WHERE id <= (SELECT COALESCE(MAX(id),0) FROM events) - ?",
            Some(vec![EVENT_LIMIT.into()]),
        )?;
        Ok(())
    }

    fn sockets(&self) -> Value {
        json!(
            self.state
                .get_websockets()
                .iter()
                .map(|ws| {
                    json!({"attachment": ws.deserialize_attachment::<Attachment>().ok().flatten(),
                "ready_state": ws.as_ref().ready_state(),
                "buffered_amount": Reflect::get(ws.as_ref(), &"bufferedAmount".into()).ok()
                    .and_then(|v| v.as_f64())})
                })
                .collect::<Vec<_>>()
        )
    }

    fn role(ws: &WebSocket) -> Result<String> {
        ws.deserialize_attachment::<Attachment>()?
            .map(|value| value.role)
            .ok_or_else(|| Error::RustError("missing probe attachment".into()))
    }

    async fn burst(&self, mut req: Request) -> Result<Response> {
        let command: Burst = req.json().await?;
        if command.frames == 0 || command.frames > MAX_FRAMES {
            return Response::error("frame bound exceeded", 400);
        }
        let sockets = self.state.get_websockets_with_tag(&command.role);
        if sockets.len() != 1 {
            return Response::error("one matching open socket required", 409);
        }
        // A run has one burst per role, so repeated HTTP calls cannot accumulate host data.
        let marker = format!("burst-{}", command.role);
        if self.state.storage().get::<bool>(&marker).await?.is_some() {
            return Response::error("burst already issued", 409);
        }
        self.state.storage().put(&marker, true).await?;
        let ws = &sockets[0];
        let mut payload = vec![0x5a; FRAME_BYTES];
        for index in 0..command.frames {
            payload[..8].copy_from_slice(&(index as u64).to_be_bytes());
            ws.send_with_bytes(&payload)?;
        }
        drop(payload);
        self.record(
            "send_returned",
            json!({"role": command.role,
            "frames": command.frames, "bytes": command.frames * FRAME_BYTES,
            "sockets": self.sockets()}),
        )?;
        if command.close {
            ws.close(Some(1000), Some("probe close"))?;
            self.record(
                "close_requested",
                json!({"role": command.role, "sockets": self.sockets()}),
            )?;
            self.state.storage().set_alarm(CLOSE_DEADLINE).await?;
        }
        Response::from_json(&json!({"submitted": command.frames, "frame_bytes": FRAME_BYTES}))
    }
}

impl DurableObject for CloseProbe {
    fn new(state: State, _env: Env) -> Self {
        Self {
            state,
            initialized: Cell::new(false),
            boot: Date::now(),
        }
    }

    async fn fetch(&self, req: Request) -> Result<Response> {
        self.initialize()?;
        let url = req.url()?;
        match (req.method(), url.path().rsplit('/').next().unwrap_or("")) {
            (Method::Get, "ws") => {
                let role = url
                    .query_pairs()
                    .find(|(name, _)| name == "role")
                    .map(|(_, value)| value.into_owned())
                    .unwrap_or_default();
                if !matches!(role.as_str(), "slow" | "healthy" | "peer-close")
                    || req.headers().get("Upgrade")?.as_deref() != Some("websocket")
                {
                    return Response::error("valid role and WebSocket upgrade required", 400);
                }
                if self.state.get_websockets().len() >= MAX_CONNECTIONS
                    || !self.state.get_websockets_with_tag(&role).is_empty()
                {
                    return Response::error("connection bound exceeded", 409);
                }
                let pair = WebSocketPair::new()?;
                self.state
                    .accept_websocket_with_tags(&pair.server, &[&role]);
                pair.server.serialize_attachment(Attachment {
                    role: role.clone(),
                    connected_at: Date::now(),
                })?;
                self.record(
                    "connected",
                    json!({"role": role, "sockets": self.sockets()}),
                )?;
                Response::from_websocket(pair.client)
            }
            (Method::Post, "burst") => self.burst(req).await,
            (Method::Post, "reset" | "prepare-reset") => {
                if self
                    .state
                    .storage()
                    .get::<bool>("reset-issued")
                    .await?
                    .is_some()
                {
                    return Response::error("reset already issued", 409);
                }
                self.state.storage().put("reset-issued", true).await?;
                self.record("reset_requested", json!({"sockets": self.sockets()}))?;
                self.state.storage().sync().await?;
                if url.path().ends_with("/prepare-reset") {
                    return Response::ok("reset prepared; Rust future may finish before abort");
                }
                // workers-rs has no high-level binding for this platform teardown primitive.
                let raw = self.state.as_raw();
                let abort =
                    Reflect::get(raw.as_ref(), &"abort".into())?.dyn_into::<js_sys::Function>()?;
                let options = js_sys::Object::new();
                Reflect::set(&options, &"retryAlarm".into(), &false.into())?;
                abort.call2(
                    raw.as_ref(),
                    &"bounded close probe escalation".into(),
                    &options,
                )?;
                Response::error("abort unexpectedly returned", 500)
            }
            (Method::Get, "status") => {
                #[derive(Deserialize)]
                struct Row {
                    detail: String,
                }
                let rows: Vec<Row> = self
                    .state
                    .storage()
                    .sql()
                    .exec("SELECT detail FROM events ORDER BY id", None)?
                    .to_array()?;
                let events: Vec<Value> = rows
                    .iter()
                    .map(|row| serde_json::from_str(&row.detail))
                    .collect::<std::result::Result<_, _>>()?;
                Response::from_json(&json!({"events": events, "sockets": self.sockets(),
                    "boot": self.boot, "physical_release_proven": false}))
            }
            _ => Response::error("not found", 404),
        }
    }

    async fn websocket_message(
        &self,
        ws: WebSocket,
        message: WebSocketIncomingMessage,
    ) -> Result<()> {
        match message {
            WebSocketIncomingMessage::String(value) if value == "ping" => ws.send_with_str("pong"),
            _ => {
                self.record("unexpected_input", json!({"role": Self::role(&ws)?}))?;
                ws.close(Some(1008), Some("probe accepts ping only"))
            }
        }
    }

    async fn websocket_close(
        &self,
        ws: WebSocket,
        code: usize,
        reason: String,
        was_clean: bool,
    ) -> Result<()> {
        self.record(
            "close_callback",
            json!({"role": Self::role(&ws)?, "code": code,
            "reason": reason, "was_clean": was_clean, "ready_state": ws.as_ref().ready_state(),
            "sockets": self.sockets()}),
        )?;
        // Record a request independently: neither this return nor CLOSED proves drained buffers.
        ws.close(Some(1000), Some("probe acknowledged"))?;
        self.record(
            "close_callback_returning",
            json!({"role": Self::role(&ws)?,
            "ready_state": ws.as_ref().ready_state()}),
        )
    }

    async fn websocket_error(&self, ws: WebSocket, error: Error) -> Result<()> {
        self.record(
            "error_callback",
            json!({"role": Self::role(&ws)?,
            "error": error.to_string(), "ready_state": ws.as_ref().ready_state()}),
        )
    }

    async fn alarm(&self) -> Result<Response> {
        self.record(
            "close_deadline",
            json!({"sockets": self.sockets(),
            "decision": "retain_unproven_occupancy"}),
        )?;
        Response::ok("closure evidence recorded; no release receipt manufactured")
    }
}
