//! Opt-in host experiment. Fixture authentication and control endpoints are not a relay API.
mod fixture;
mod persistence;
mod runtime;

use fixture::Fixture;
use persistence::{Image, Mode};
use runtime::{Runtime, Trace};
use serde::Deserialize;
use serde_json::json;
use std::{cell::RefCell, num::NonZeroU64};
use windshare_relay_core::{
    identifiers::RelaySessionId,
    lifecycle::ConnectionRef,
    sessions::SessionAdmitted,
    share::{Authority, Input, Responsibility, Restore, ShareRelay},
};
use worker::worker_sys::ext::WebSocketExt;
use worker::*;

fn failure(error: impl std::fmt::Debug) -> Error {
    Error::RustError(format!("{error:?}"))
}

enum Loaded {
    Cold,
    Ready(Box<Runtime>),
    Blocked(String),
}

#[durable_object]
pub struct CheckpointProbe {
    state: State,
    boot: String,
    incarnation: [u8; 16],
    loaded: RefCell<Loaded>,
}

impl CheckpointProbe {
    fn trace(&self, event: &str, detail: serde_json::Value) {
        console_log!(
            "{}",
            json!({"event":event, "object":self.state.id().to_string(),
            "boot":self.boot, "at_ms":js_sys::Date::now(), "detail":detail})
        );
    }

    fn initialize(&self, fresh: Option<Mode>) -> Result<()> {
        if !matches!(*self.loaded.borrow(), Loaded::Cold) {
            return Ok(());
        }
        let result = self.restore(fresh);
        match result {
            Ok(runtime) => {
                self.trace("checkpoint_restore", runtime.status(&self.state)?);
                *self.loaded.borrow_mut() = Loaded::Ready(Box::new(runtime));
            }
            Err(error) => {
                self.trace(
                    "checkpoint_restore_refused",
                    json!({"reason":error.to_string()}),
                );
                *self.loaded.borrow_mut() = Loaded::Blocked(error.to_string());
            }
        }
        Ok(())
    }

    fn restore(&self, fresh: Option<Mode>) -> Result<Runtime> {
        let stored = persistence::load(&self.state)?;
        if stored.is_some() && fresh.is_some() {
            return Err(failure("already configured"));
        }
        let fixture = Fixture::new();
        let image = match stored {
            Some(image) => image,
            None => Image::empty(fresh.ok_or_else(|| failure("configure the probe first"))?),
        };
        let checkpoint = if fresh.is_some() {
            None
        } else {
            Some(image.recover(&self.state)?)
        };
        let surviving: Vec<_> = self
            .state
            .get_websockets()
            .iter()
            .filter(|ws| ws.as_ref().ready_state() == 1)
            .map(|ws| self.connection(ws))
            .collect::<Result<_>>()?;
        let restored = Restore {
            authority: Authority::Known(image.record()?),
            surviving: &surviving,
            responsibility: checkpoint.as_ref().map_or(
                Responsibility::Released { previous: None },
                Responsibility::Checkpoint,
            ),
        };
        let (mut relay, turn) = ShareRelay::restore(
            fixture.identity(self.incarnation),
            fixture::limits(),
            &restored,
            Trace {
                object: self.state.id().to_string(),
                boot: self.boot.clone(),
            },
            fixture.now(),
        )
        .map_err(failure)?;
        relay
            .enable_checkpoints(fixture::budget(), fixture.now())
            .map_err(failure)?;
        let mut runtime = Runtime {
            relay,
            image,
            fixture,
            last_refusal: None,
        };
        runtime.outputs(&self.state, turn, None)?;
        runtime.flush(&self.state)?;
        Ok(runtime)
    }

    fn connection(&self, ws: &WebSocket) -> Result<ConnectionRef> {
        // Tags identify the transport even when a deliberately erased attachment is missing.
        self.state
            .get_tags(ws)
            .iter()
            .find_map(|tag| {
                let mut fields = tag.strip_prefix("c-")?.split('-');
                let id = NonZeroU64::new(fields.next()?.parse().ok()?)?;
                let generation = NonZeroU64::new(fields.next()?.parse().ok()?)?;
                (fields.next().is_none()).then_some(ConnectionRef { id, generation })
            })
            .ok_or_else(|| failure("unknown connection tag"))
    }

    fn allocate(&self, id: u64) -> Result<ConnectionRef> {
        let mut loaded = self.loaded.borrow_mut();
        let Loaded::Ready(runtime) = &mut *loaded else {
            return Err(failure("responsibility unavailable"));
        };
        let generation = runtime.image.last_generation[(id - 1) as usize]
            .checked_add(1)
            .ok_or_else(|| failure("connection generations exhausted"))?;
        runtime.image.last_generation[(id - 1) as usize] = generation;
        // Persist the allocator before accepting a replacement transport. Runtime
        // reconstruction must never reuse a lifetime still named by an old ledger.
        persistence::save(&self.state, &mut runtime.image)?;
        Ok(ConnectionRef {
            id: NonZeroU64::new(id).unwrap(),
            generation: NonZeroU64::new(generation).unwrap(),
        })
    }

    fn mutate(&self, action: impl FnOnce(&mut Runtime) -> Result<()>) -> Result<()> {
        self.initialize(None)?;
        let mut loaded = self.loaded.borrow_mut();
        let Loaded::Ready(runtime) = &mut *loaded else {
            return Err(failure("responsibility unavailable"));
        };
        let result = runtime
            .begin(&self.state)
            .and_then(|()| action(runtime))
            .and_then(|()| runtime.flush(&self.state));
        if let Err(error) = &result {
            runtime.image.pending = true;
            if let Err(marker_error) = persistence::save(&self.state, &mut runtime.image) {
                self.trace(
                    "checkpoint_failure_marker_failed",
                    json!({"reason":marker_error.to_string()}),
                );
            }
            // Never continue with a flush acknowledged only in memory. The mirror's
            // pending marker prevents a later constructor accepting the older image.
            self.trace(
                "checkpoint_event_failed",
                json!({"reason":error.to_string()}),
            );
            *loaded = Loaded::Blocked(error.to_string());
        }
        result
    }

    fn status(&self) -> Result<Response> {
        self.initialize(None)?;
        let loaded = self.loaded.borrow();
        let mut status = match &*loaded {
            Loaded::Ready(runtime) => runtime.status(&self.state)?,
            Loaded::Blocked(reason) => json!({"ready":false, "reason":reason,
                "physical_release_proven":false}),
            Loaded::Cold => unreachable!(),
        };
        status["boot"] = self.boot.clone().into();
        status["sockets"] = self.state.get_websockets().len().into();
        Response::from_json(&status)
    }
}

impl DurableObject for CheckpointProbe {
    fn new(state: State, _env: Env) -> Self {
        // Runtime identity is distinct from connection generation, including normal hibernation.
        let boot = js_sys::Math::random().to_string() + "-" + &js_sys::Date::now().to_string();
        let mut incarnation = [0u8; 16];
        for byte in &mut incarnation {
            *byte = (js_sys::Math::random() * 256.0) as u8;
        }
        incarnation[0] |= 1;
        Self {
            state,
            boot,
            incarnation,
            loaded: RefCell::new(Loaded::Cold),
        }
    }

    async fn fetch(&self, mut req: Request) -> Result<Response> {
        let url = req.url()?;
        let action = url.path().rsplit('/').next().unwrap_or("");
        match (req.method(), action) {
            (Method::Post, "configure") => {
                #[derive(Deserialize)]
                struct Configure {
                    mode: Mode,
                }
                let config: Configure = req.json().await?;
                if persistence::load(&self.state)?.is_some() {
                    return Response::error("already configured", 409);
                }
                self.initialize(Some(config.mode))?;
                self.status()
            }
            (Method::Get, "status") => self.status(),
            (Method::Get, "ws") => {
                self.initialize(None)?;
                let role = url
                    .query_pairs()
                    .find(|(key, _)| key == "role")
                    .map(|(_, value)| value.into_owned())
                    .unwrap_or_default();
                let id = match role.as_str() {
                    "sender" => fixture::SENDER,
                    "receiver" => fixture::RECEIVER,
                    "receiver2" => fixture::SECOND_RECEIVER,
                    _ => return Response::error("sender, receiver or receiver2 required", 400),
                };
                if req.headers().get("Upgrade")?.as_deref() != Some("websocket") {
                    return Response::error("websocket required", 400);
                }
                if !matches!(*self.loaded.borrow(), Loaded::Ready(_))
                    || self
                        .state
                        .get_websockets_with_tag(&role)
                        .iter()
                        .any(|ws| ws.as_ref().ready_state() == 1)
                {
                    return Response::error("connection unavailable", 409);
                }
                let connection = self.allocate(id)?;
                let pair = WebSocketPair::new()?;
                self.state.accept_websocket_with_tags(
                    &pair.server,
                    &[&persistence::tag(connection), &role],
                );
                if let Err(error) = self.mutate(|runtime| {
                    if id == fixture::SENDER {
                        runtime.register(&self.state, connection)
                    } else {
                        runtime.join(&self.state, connection)
                    }
                }) {
                    pair.server.close(Some(1011), Some("checkpoint failed"))?;
                    return Err(error);
                }
                if let Loaded::Ready(runtime) = &*self.loaded.borrow()
                    && let Some(reason) = &runtime.last_refusal
                {
                    pair.server.close(Some(1008), Some("admission refused"))?;
                    return Response::error(reason, 409);
                }
                Response::from_websocket(pair.client)
            }
            (Method::Post, "lose-attachment") => {
                let ws = persistence::socket(&self.state, fixture::connection(fixture::RECEIVER))
                    .ok_or_else(|| failure("receiver missing"))?;
                ws.as_ref()
                    .serialize_attachment(wasm_bindgen::JsValue::NULL)?;
                self.trace(
                    "checkpoint_attachment_erased",
                    json!({"connection":"c-2-1"}),
                );
                Response::ok("receiver attachment erased without flushing the runtime")
            }
            (Method::Post, "cancel") => {
                self.mutate(|runtime| {
                    runtime.apply(
                        &self.state,
                        Input::Cancel {
                            participant: fixture::connection(fixture::SENDER),
                            session: RelaySessionId::new(1_u64.to_be_bytes()).unwrap(),
                        },
                        None,
                    )
                })?;
                self.status()
            }
            (Method::Post, "prepare-reset") => {
                // Do not flush here: that would hide a lost hot-path update.
                self.state.storage().sync().await?;
                Response::ok("reset may run after this Rust future finishes")
            }
            _ => Response::error("not found", 404),
        }
    }

    async fn websocket_message(
        &self,
        ws: WebSocket,
        message: WebSocketIncomingMessage,
    ) -> Result<()> {
        let connection = self.connection(&ws)?;
        if let Err(error) = self.mutate(|runtime| {
            match &message {
                WebSocketIncomingMessage::String(value) if value == "ping" => {
                    ws.send_with_str("pong")?
                }
                WebSocketIncomingMessage::Binary(bytes) if bytes.starts_with(b"WS2O") => {
                    runtime.forward(&self.state, connection, bytes)?;
                }
                WebSocketIncomingMessage::Binary(bytes) if bytes.starts_with(b"WS2M") => {
                    runtime.apply(
                        &self.state,
                        Input::Admitted {
                            sender: connection,
                            frame: SessionAdmitted::decode(bytes).map_err(failure)?,
                        },
                        None,
                    )?;
                }
                WebSocketIncomingMessage::Binary(bytes) => {
                    runtime.apply(
                        &self.state,
                        Input::Control {
                            connection,
                            frame: bytes,
                        },
                        None,
                    )?;
                }
                _ => return Err(failure("unexpected probe message")),
            }
            Ok(())
        }) {
            ws.send_with_str(
                json!({"event":"recovery_blocked","reason":error.to_string()}).to_string(),
            )?;
        }
        Ok(())
    }

    async fn websocket_close(
        &self,
        ws: WebSocket,
        _code: usize,
        _reason: String,
        _clean: bool,
    ) -> Result<()> {
        let connection = self.connection(&ws)?;
        self.trace(
            "checkpoint_disconnected",
            json!({"connection":persistence::tag(connection)}),
        );
        ws.close(Some(1000), Some("probe acknowledged"))?;
        // Socket closure is communication evidence only; no Input::Closed is synthesized.
        let _ = self
            .mutate(|runtime| runtime.apply(&self.state, Input::Disconnected(connection), None));
        Ok(())
    }

    async fn websocket_error(&self, ws: WebSocket, error: Error) -> Result<()> {
        self.trace(
            "checkpoint_socket_error",
            json!({"reason":error.to_string()}),
        );
        let connection = self.connection(&ws)?;
        let _ = self
            .mutate(|runtime| runtime.apply(&self.state, Input::Disconnected(connection), None));
        Ok(())
    }
}
