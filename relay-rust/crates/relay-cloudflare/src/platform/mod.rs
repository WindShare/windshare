#![allow(
    clippy::future_not_send,
    reason = "Workers execute JS futures on one isolate thread"
)]

mod edge;
mod io;
mod recovery;
mod storage;
mod trace;

use crate::{config, persistence, runtime::Host};
use anyhow::{Result as HostResult, anyhow};
use io::CloudflareHost;
use recovery::Ready;
use serde_json::json;
use std::cell::RefCell;
use storage::{Database, IMAGE, api};
use windshare_relay_core::share::Input;
use worker::{
    DurableObject, Env, Method, Request, Response, Result, State, WebSocket,
    WebSocketIncomingMessage, WebSocketPair, durable_object,
};

const RECOVERY_RETRY_MS: u64 = 1_000;

enum Loaded {
    Cold,
    Ready(Box<Ready>),
}

#[durable_object]
pub struct ShareRelayObject {
    state: State,
    env: Env,
    loaded: RefCell<Loaded>,
}

fn failure(error: impl std::fmt::Display) -> worker::Error {
    worker::Error::RustError(error.to_string())
}

impl ShareRelayObject {
    fn initialize(&self, route: Option<&config::Route>) -> HostResult<()> {
        let mut loaded = self.loaded.borrow_mut();
        if matches!(*loaded, Loaded::Cold) {
            *loaded = Loaded::Ready(Box::new(recovery::restore(&self.state, route)?));
        }
        Ok(())
    }

    fn with_runtime<T>(
        &self,
        action: impl FnOnce(&mut Ready, &mut CloudflareHost<'_>) -> HostResult<T>,
    ) -> HostResult<T> {
        let mut loaded = self.loaded.borrow_mut();
        let Loaded::Ready(ready) = &mut *loaded else {
            return Err(anyhow!("host is not restored"));
        };
        let mut host = CloudflareHost {
            state: &self.state,
            database: Database::open(&self.state)?,
            identity: ready.identity,
        };
        action(ready, &mut host)
    }

    fn recover_after_failure(&self, error: &anyhow::Error) {
        worker::console_log!(
            "{}",
            json!({
                "platform": "cloudflare", "event": "host_recovery_required",
                "object": self.state.id().to_string(), "reason": error.to_string(),
            })
        );
        // An uncertain authority/inventory cannot authorize any socket. A new
        // event reads SQLite afresh and completes unresolved kernel obligations.
        *self.loaded.borrow_mut() = Loaded::Cold;
        for ws in self.state.get_websockets() {
            let _ = ws.close(Some(1011), Some("authority recovery required"));
        }
    }

    async fn finish<T>(&self, result: HostResult<T>) -> Result<T> {
        if let Err(error) = &result {
            self.recover_after_failure(error);
        }
        if let Err(error) = self.schedule().await {
            self.recover_after_failure(&anyhow!("{error}"));
            return Err(error);
        }
        result.map_err(failure)
    }

    async fn schedule(&self) -> Result<()> {
        // Storage reads retain the default input gate. Re-read the in-memory
        // deadline after awaiting; never install a stale event's later deadline.
        let scheduled = self.state.storage().get_alarm().await?;
        let deadline = match &*self.loaded.borrow() {
            Loaded::Ready(ready) => ready.runtime.deadline().map(|at| at.0),
            Loaded::Cold => Some(io::now().0.saturating_add(RECOVERY_RETRY_MS)),
        };
        if let Some(at) =
            config::earlier_alarm(scheduled.and_then(|at| u64::try_from(at).ok()), deadline)
        {
            self.state
                .storage()
                .set_alarm(io::alarm_time(at).map_err(failure)?)
                .await?;
        }
        Ok(())
    }

    fn accept(&self, req: &Request, route: &config::Route) -> HostResult<Response> {
        self.initialize(Some(route))?;
        self.with_runtime(|ready, host| {
            // Existing authority is restored independently of this request. A
            // different public hostname must not evict its healthy connections.
            if ready.image.check_route(route).is_err() {
                return api(Response::error("relay endpoint identity mismatch", 400));
            }
            let connection = ready.image.allocate()?;
            host.database.put(IMAGE, &ready.image)?;
            let pair = api(WebSocketPair::new())?;
            let tag = persistence::tag(connection);
            let may_register =
                api(req.headers().get(edge::REGISTER_HEADER))?.as_deref() == Some("1");
            let mut tags = vec![tag.as_str()];
            if may_register {
                tags.push(edge::REGISTER_TAG);
            }
            self.state.accept_websocket_with_tags(&pair.server, &tags);
            ready.runtime.event(host, Input::Connected(connection))?;
            api(Response::from_websocket(pair.client))
        })
    }

    fn disconnected(&self, ws: &WebSocket) -> HostResult<()> {
        self.initialize(None)?;
        let connection = io::connection(&self.state, ws)?;
        self.with_runtime(|ready, host| {
            host.close(connection)?;
            ready.runtime.event(host, Input::Disconnected(connection))
        })
    }
}

impl DurableObject for ShareRelayObject {
    fn new(state: State, env: Env) -> Self {
        Self {
            state,
            env,
            loaded: RefCell::new(Loaded::Cold),
        }
    }

    async fn fetch(&self, req: Request) -> Result<Response> {
        if req.method() != Method::Get
            || !req
                .headers()
                .get("Upgrade")?
                .is_some_and(|v| v.eq_ignore_ascii_case("websocket"))
        {
            return Response::error("WebSocket upgrade required", 426);
        }
        let route = config::Route::parse(req.url()?.as_str()).map_err(failure)?;
        let namespace = self.env.durable_object(edge::NAMESPACE)?;
        let expected = namespace.id_from_name(&route.object_name())?;
        if expected.to_string() != self.state.id().to_string() {
            return Response::error("route selects a different object", 400);
        }
        self.finish(self.accept(&req, &route)).await
    }

    async fn websocket_message(
        &self,
        ws: WebSocket,
        message: WebSocketIncomingMessage,
    ) -> Result<()> {
        let result = (|| {
            self.initialize(None)?;
            let connection = io::connection(&self.state, &ws)?;
            let may_register = self
                .state
                .get_tags(&ws)
                .iter()
                .any(|tag| tag == edge::REGISTER_TAG);
            self.with_runtime(|ready, host| match message {
                WebSocketIncomingMessage::Binary(bytes) => {
                    ready
                        .runtime
                        .control(host, connection, &bytes, may_register)
                }
                WebSocketIncomingMessage::String(_) => {
                    host.close(connection)?;
                    ready.runtime.event(host, Input::Disconnected(connection))
                }
            })
        })();
        self.finish(result).await
    }

    async fn websocket_close(
        &self,
        ws: WebSocket,
        _code: usize,
        _reason: String,
        _clean: bool,
    ) -> Result<()> {
        self.finish(self.disconnected(&ws)).await
    }

    async fn websocket_error(&self, ws: WebSocket, error: worker::Error) -> Result<()> {
        worker::console_log!(
            "{}",
            json!({
                "platform": "cloudflare", "event": "socket_error",
                "object": self.state.id().to_string(), "reason": error.to_string(),
            })
        );
        self.finish(self.disconnected(&ws)).await
    }

    async fn alarm(&self) -> Result<Response> {
        let result = (|| {
            self.initialize(None)?;
            self.with_runtime(|ready, host| ready.runtime.event(host, Input::Tick))?;
            api(Response::empty())
        })();
        self.finish(result).await
    }
}
