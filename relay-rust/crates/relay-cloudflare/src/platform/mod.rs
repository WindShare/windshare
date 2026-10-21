#![allow(
    clippy::future_not_send,
    reason = "Workers execute JS futures on one isolate thread"
)]

mod access;
mod edge;
pub use access::AccessObject;
mod io;
mod recovery;
mod storage;
mod trace;

use crate::{config, persistence, runtime::Host};
use anyhow::{Result as HostResult, anyhow};
use io::CloudflareHost;
use recovery::Ready;
use serde_json::json;
use std::cell::{Cell, RefCell};
use storage::{IMAGE, api};
use windshare_relay_core::{
    access::{RegistrationDecision, RegistrationRequest},
    ingress::{Control, FrameKind},
    share::Input,
};
use worker::{
    DurableObject, Env, Method, Request, Response, Result, State, WebSocket,
    WebSocketIncomingMessage, WebSocketPair, durable_object,
};

const RECOVERY_RETRY_MS: u64 = 1_000;

enum Loaded {
    Cold,
    Ready(Box<Ready>),
}

#[derive(Clone, Copy)]
enum AlarmState {
    Unknown,
    Known(Option<u64>),
}

#[durable_object]
pub struct ShareRelayObject {
    state: State,
    env: Env,
    loaded: RefCell<Loaded>,
    alarm: Cell<AlarmState>,
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
            database: ready.database.clone(),
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
        self.alarm.set(AlarmState::Unknown);
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
        // Read once after construction, not for every payload. Storage retains
        // its input gate; use the current kernel deadline after any await.
        let scheduled = match self.alarm.get() {
            AlarmState::Known(at) => at,
            AlarmState::Unknown => {
                let at = self
                    .state
                    .storage()
                    .get_alarm()
                    .await?
                    .and_then(|at| u64::try_from(at).ok());
                self.alarm.set(AlarmState::Known(at));
                at
            }
        };
        let deadline = match &*self.loaded.borrow() {
            Loaded::Ready(ready) => ready.runtime.deadline().map(|at| at.0),
            Loaded::Cold => Some(io::now().0.saturating_add(RECOVERY_RETRY_MS)),
        };
        if let Some(at) = config::earlier_alarm(scheduled, deadline) {
            self.state
                .storage()
                .set_alarm(io::alarm_time(at).map_err(failure)?)
                .await?;
            self.alarm.set(AlarmState::Known(Some(at)));
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
            let digest = api(req.headers().get(access::CREDENTIAL_HEADER))?;
            let credential_tag = digest.map(|digest| format!("{}{digest}", access::CREDENTIAL_TAG));
            let mut tags = vec![tag.as_str()];
            if let Some(credential_tag) = &credential_tag {
                tags.push(credential_tag.as_str());
            }
            self.state.accept_websocket_with_tags(&pair.server, &tags);
            ready.runtime.event(host, Input::Connected(connection))?;
            api(Response::from_websocket(pair.client))
        })
    }

    async fn registration_access(
        &self,
        ws: &WebSocket,
        request: &RegistrationRequest,
    ) -> RegistrationDecision {
        let connection = request.connection();
        // Authorization records drive revocation. Bind their target before any
        // remote side effect, using the object's restored identity as authority.
        let route = request.registration().route();
        let matches_object = matches!(&*self.loaded.borrow(), Loaded::Ready(ready)
            if route.share_id() == ready.identity.share);
        if !matches_object {
            worker::console_log!(
                "{}",
                json!({"event":"registration_route_refused",
                "operation_id":persistence::tag(connection),
                "share_id":access::text_id(route.share_id().as_bytes())})
            );
            return RegistrationDecision::Denied;
        }
        let digest = self
            .state
            .get_tags(ws)
            .iter()
            .find_map(|tag| tag.strip_prefix(access::CREDENTIAL_TAG).map(str::to_string));
        let Some(digest) = digest else {
            return RegistrationDecision::Denied;
        };
        let body = json!({"digest":digest, "share_id":access::text_id(request.registration().route().share_id().as_bytes()),
            "operation_id":persistence::tag(connection)});
        let resolve = async {
            let request = Request::new_with_init(
                "https://internal/internal/authorize",
                worker::RequestInit::new()
                    .with_method(Method::Post)
                    .with_body(Some(body.to_string().into())),
            )?;
            let mut response = access::fetch(&self.env, request).await?;
            if response.status_code() == 403 {
                return Ok(RegistrationDecision::Denied);
            }
            if response.status_code() != 200 {
                return Ok(RegistrationDecision::Unavailable);
            }
            let value: serde_json::Value = response.json().await?;
            let attribution =
                serde_json::from_value(value["attribution"].clone()).map_err(failure)?;
            Ok::<_, worker::Error>(RegistrationDecision::Granted(attribution))
        };
        match resolve.await {
            Ok(access) => access,
            Err(error) => {
                worker::console_log!(
                    "{}",
                    json!({"event":"registration_access_unavailable",
                    "operation_id":persistence::tag(connection),"share_id":access::text_id(request.registration().route().share_id().as_bytes()),"reason":error.to_string()})
                );
                RegistrationDecision::Unavailable
            }
        }
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
            alarm: Cell::new(AlarmState::Unknown),
        }
    }

    async fn fetch(&self, mut req: Request) -> Result<Response> {
        if req.method() == Method::Post && req.path() == "/internal/terminate" {
            let body: serde_json::Value = req.json().await?;
            let credential = access::parse_id::<16>(body["credential_id"].as_str().unwrap_or(""))?;
            let database = storage::Database::open(&self.state).map_err(failure)?;
            // Empty targets need no runtime, but must retain the same fence as
            // live targets. Unknown or damaged authority must still fail closed.
            if database.uninitialized().map_err(failure)? && self.state.get_websockets().is_empty()
            {
                database
                    .put(
                        &format!("access-stop:{}", access::text_id(&credential)),
                        &true,
                    )
                    .map_err(failure)?;
                return Response::from_json(&json!({"ok":true}));
            }
            let stop_id =
                windshare_relay_core::identifiers::StopId::new(io::random().map_err(failure)?)
                    .ok_or_else(|| failure("zero stop identifier"))?;
            let result = (|| {
                self.initialize(None)?;
                self.with_runtime(|ready, host| {
                    // Fence an authorization response still in flight when revocation arrived.
                    ready.database.put(
                        &format!("access-stop:{}", access::text_id(&credential)),
                        &true,
                    )?;
                    ready.runtime.event(
                        host,
                        Input::Terminate {
                            credential,
                            stop_id,
                        },
                    )?;
                    api(Response::from_json(&json!({"ok":true})))
                })
            })();
            return self.finish(result).await;
        }
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
        self.initialize(None).map_err(failure)?;
        let connection = io::connection(&self.state, &ws).map_err(failure)?;
        let result = match message {
            WebSocketIncomingMessage::Binary(bytes)
                if FrameKind::classify(&bytes) == FrameKind::Opaque =>
            {
                self.with_runtime(|ready, host| ready.runtime.forward(host, connection, bytes))
            }
            WebSocketIncomingMessage::Binary(bytes) => {
                let control = if bytes.len() > config::MAX_CONTROL_BYTES {
                    Control::Ready(Input::Unsupported(connection))
                } else {
                    Control::inspect(connection, &bytes)
                };
                match control {
                    Control::Ready(input) => {
                        self.with_runtime(|ready, host| ready.runtime.event(host, input))
                    }
                    Control::Authorize(request) => {
                        let decision = self.registration_access(&ws, &request).await;
                        self.with_runtime(|ready, host| {
                            let decision = match decision {
                                RegistrationDecision::Granted(
                                    windshare_relay_core::access::Attribution::Credential {
                                        credential,
                                        ..
                                    },
                                ) if ready
                                    .database
                                    .get::<bool>(&format!(
                                        "access-stop:{}",
                                        access::text_id(&credential)
                                    ))?
                                    .unwrap_or(false) =>
                                {
                                    RegistrationDecision::Denied
                                }
                                decision => decision,
                            };
                            let authorization = request.resolve(decision, host.now());
                            ready.runtime.event(host, Input::Register(authorization))
                        })
                    }
                }
            }
            WebSocketIncomingMessage::String(_) => self.with_runtime(|ready, host| {
                host.close(connection)?;
                ready.runtime.event(host, Input::Disconnected(connection))
            }),
        };
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
        self.alarm.set(AlarmState::Known(None));
        let result = (|| {
            self.initialize(None)?;
            self.with_runtime(|ready, host| ready.runtime.event(host, Input::Tick))?;
            api(Response::empty())
        })();
        self.finish(result).await
    }
}
