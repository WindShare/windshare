//! Bounded host pressure, deliberately independent of production admission policy.
use js_sys::Date;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use worker::*;

#[path = "../../payload.rs"]
mod payload;

const MAX_CONNECTIONS: u64 = 96;
const MAX_SLOW_FRAMES: u64 = 8192; // 512 MiB over the entire object, including resets.
const MAX_BURST_FRAMES: u64 = 256;
const MAX_ECHO_FRAMES: u64 = 1024;
const MAX_RESETS: u64 = 32;

#[derive(Serialize, Deserialize)]
struct Attachment {
    role: String,
    echoes: u64,
}

#[derive(Deserialize)]
struct Burst {
    role: String,
    frames: u64,
    close: bool,
}

#[derive(Deserialize)]
struct Budget {
    connections: u64,
    frames: u64,
    resets: u64,
}

#[durable_object]
pub struct PressureSockets {
    state: State,
    boot: f64,
}

impl PressureSockets {
    fn budget(&self) -> Result<Budget> {
        self.state.storage().sql().exec(
            "CREATE TABLE IF NOT EXISTS budget (id INTEGER PRIMARY KEY CHECK(id=1), connections INTEGER NOT NULL, frames INTEGER NOT NULL, resets INTEGER NOT NULL)", None)?;
        self.state
            .storage()
            .sql()
            .exec("INSERT OR IGNORE INTO budget VALUES(1,0,0,0)", None)?;
        self.state.storage().sql().exec(
            "CREATE TABLE IF NOT EXISTS issued (role TEXT PRIMARY KEY)",
            None,
        )?;
        self.state
            .storage()
            .sql()
            .exec(
                "SELECT connections,frames,resets FROM budget WHERE id=1",
                None,
            )?
            .one()
    }

    fn record(&self, event: &str, detail: Value) {
        console_log!(
            "{}",
            json!({"probe":"pressure", "event":event, "at_ms":Date::now(),
            "object":self.state.id().to_string(), "rust_boot":self.boot, "detail":detail})
        );
    }

    fn sockets(&self) -> Value {
        json!(
            self.state
                .get_websockets()
                .iter()
                .map(|ws| json!({
                    "attachment":ws.deserialize_attachment::<Attachment>().ok().flatten(),
                    "ready_state":ws.as_ref().ready_state()
                }))
                .collect::<Vec<_>>()
        )
    }

    async fn burst(&self, mut req: Request) -> Result<Response> {
        let command: Burst = req.json().await?;
        let budget = self.budget()?;
        if !command.role.starts_with("slow-")
            || command.frames == 0
            || command.frames > MAX_BURST_FRAMES
            || budget.frames + command.frames > MAX_SLOW_FRAMES
        {
            return Response::error("burst bound exceeded", 400);
        }
        let sockets = self.state.get_websockets_with_tag(&command.role);
        if sockets.len() != 1 {
            return Response::error("one matching socket required", 409);
        }
        // Reserve the entire attempt durably before emitting payload: retries and resets
        // cannot replenish this experiment's traffic budget.
        self.state.storage().sql().exec(
            "INSERT INTO issued VALUES (?)",
            Some(vec![command.role.clone().into()]),
        )?;
        self.state.storage().sql().exec(
            "UPDATE budget SET frames=frames+? WHERE id=1",
            Some(vec![(command.frames as i64).into()]),
        )?;
        self.state.storage().sync().await?;
        let mut bytes = vec![0; payload::FRAME_BYTES];
        for index in 0..command.frames {
            payload::fill_frame(&mut bytes, index);
            sockets[0].send_with_bytes(&bytes)?;
        }
        if command.close {
            sockets[0].close(Some(1000), Some("pressure probe close"))?;
        }
        self.record(
            "burst_returned",
            json!({"role":command.role, "frames":command.frames,
            "cumulative_reserved_frames":budget.frames+command.frames, "close":command.close,
            "socket_state":sockets[0].as_ref().ready_state()}),
        );
        Response::from_json(
            &json!({"frames":command.frames, "frame_bytes":payload::FRAME_BYTES,
            "payload":payload::PAYLOAD_FORMAT, "rust_boot":self.boot}),
        )
    }
}

impl DurableObject for PressureSockets {
    fn new(state: State, _env: Env) -> Self {
        Self {
            state,
            boot: Date::now(),
        }
    }

    async fn fetch(&self, req: Request) -> Result<Response> {
        let budget = self.budget()?;
        let url = req.url()?;
        match (req.method(), url.path().rsplit('/').next().unwrap_or("")) {
            (Method::Get, "ws") => {
                let role = url
                    .query_pairs()
                    .find(|(k, _)| k == "role")
                    .map(|(_, v)| v.into_owned())
                    .unwrap_or_default();
                if role.len() > 32
                    || !(role.starts_with("slow-") || role.starts_with("healthy-"))
                    || !role.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-')
                    || req.headers().get("Upgrade")?.as_deref() != Some("websocket")
                {
                    return Response::error("invalid socket role", 400);
                }
                if budget.connections >= MAX_CONNECTIONS
                    || !self.state.get_websockets_with_tag(&role).is_empty()
                {
                    return Response::error("connection bound exceeded", 409);
                }
                self.state.storage().sql().exec(
                    "UPDATE budget SET connections=connections+1 WHERE id=1",
                    None,
                )?;
                let pair = WebSocketPair::new()?;
                self.state
                    .accept_websocket_with_tags(&pair.server, &[&role]);
                pair.server.serialize_attachment(Attachment {
                    role: role.clone(),
                    echoes: 0,
                })?;
                self.record("connected", json!({"role":role}));
                Response::from_websocket(pair.client)
            }
            (Method::Post, "burst") => self.burst(req).await,
            (Method::Post, "prepare-reset") => {
                if budget.resets >= MAX_RESETS {
                    return Response::error("reset bound exceeded", 409);
                }
                self.state
                    .storage()
                    .sql()
                    .exec("UPDATE budget SET resets=resets+1 WHERE id=1", None)?;
                self.record("reset_prepared", json!({"resets":budget.resets+1}));
                self.state.storage().sync().await?;
                Response::ok("prepared")
            }
            (Method::Get, "status") => Response::from_json(&json!({
                "rust_boot":self.boot, "object":self.state.id().to_string(),
                "connections_ever":budget.connections, "reserved_frames":budget.frames,
                "resets":budget.resets, "sockets":self.sockets(),
                "physical_release_proven":false
            })),
            _ => Response::error("not found", 404),
        }
    }

    async fn websocket_message(
        &self,
        ws: WebSocket,
        message: WebSocketIncomingMessage,
    ) -> Result<()> {
        let Some(mut attachment) = ws.deserialize_attachment::<Attachment>()? else {
            return ws.close(Some(1008), Some("missing attachment"));
        };
        match message {
            WebSocketIncomingMessage::String(value)
                if value.starts_with("ping:") && value.len() < 80 =>
            {
                ws.send_with_str(value.replacen("ping:", "pong:", 1))
            }
            WebSocketIncomingMessage::Binary(bytes)
                if attachment.role.starts_with("healthy-")
                    && bytes.len() == payload::FRAME_BYTES
                    && attachment.echoes < MAX_ECHO_FRAMES =>
            {
                attachment.echoes += 1;
                ws.serialize_attachment(attachment)?;
                ws.send_with_bytes(&bytes)
            }
            _ => ws.close(Some(1008), Some("probe input bound exceeded")),
        }
    }

    async fn websocket_close(
        &self,
        ws: WebSocket,
        code: usize,
        reason: String,
        clean: bool,
    ) -> Result<()> {
        self.record(
            "close_callback",
            json!({"attachment":ws.deserialize_attachment::<Attachment>()?,
            "code":code, "reason":reason, "clean":clean}),
        );
        ws.close(Some(1000), Some("acknowledged"))
    }

    async fn websocket_error(&self, ws: WebSocket, error: Error) -> Result<()> {
        self.record(
            "error_callback",
            json!({"attachment":ws.deserialize_attachment::<Attachment>()?,
            "error":error.to_string()}),
        );
        Ok(())
    }
}
