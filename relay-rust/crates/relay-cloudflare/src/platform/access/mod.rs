mod handlers;
mod sessions;
mod storage;
mod store;
mod termination;
mod turn;

use super::io;
use serde_json::{Value, json};
use std::cell::{Cell, RefCell};
use storage::Database;
use windshare_relay_console::{
    MAX_BODY_BYTES, MAX_RECORDS,
    http::{
        ConsoleError, STATUS_METHOD_NOT_ALLOWED, STATUS_PAYLOAD_TOO_LARGE,
        STATUS_SERVICE_UNAVAILABLE, check_mutation_headers, is_claimant_endpoint,
    },
    text_id,
};
use worker::{DurableObject, Env, Method, Request, Response, Result, State, durable_object};

pub const NAMESPACE: &str = "ACCESS";
pub const OBJECT_NAME: &str = "authority";
pub const CREDENTIAL_HEADER: &str = "x-windshare-credential-digest";
pub const CREDENTIAL_TAG: &str = "credential:";
const CLEANUP_INTERVAL_MS: u64 = 60_000;
const MINIMUM_ALARM_DELAY_MS: u64 = 1_000;
const UNAVAILABLE_RETRY_MS: u64 = 5_000;
const COOKIE_NAME: &str = "__Host-windshare_admin";

#[durable_object]
pub struct AccessObject {
    state: State,
    env: Env,
    schema_ready: Cell<bool>,
    turn_limits: RefCell<turn::Limits>,
}

pub fn failure(error: impl std::fmt::Display) -> worker::Error {
    worker::Error::RustError(error.to_string())
}

fn random<const N: usize>() -> Result<[u8; N]> {
    io::random().map_err(failure)
}

fn key(kind: &str, id: &[u8]) -> String {
    format!("{kind}:{}", text_id(id))
}

fn trace(event: &str, fields: &Value) {
    worker::console_log!(
        "{}",
        json!({"platform": "cloudflare", "event": event, "context": fields})
    );
}

pub(super) fn reply(value: &Value, status: u16) -> Result<Response> {
    let mut response = Response::from_json(value)?.with_status(status);
    response.headers_mut().set(
        "Cache-Control",
        windshare_relay_console::NO_STORE_CACHE_CONTROL,
    )?;
    response
        .headers_mut()
        .set("X-Content-Type-Options", "nosniff")?;
    Ok(response)
}

fn reply_error(error: ConsoleError) -> Result<Response> {
    reply(&error.body(), error.status)
}

impl AccessObject {
    fn database(&self) -> Result<Database> {
        let db = Database::new(&self.state);
        if !self.schema_ready.get() {
            // Provision tables and partial indexes once per DO instance so
            // online requests and read paths never run DDL.
            db.ensure_schema()?;
            self.schema_ready.set(true);
            trace("access_schema_ready", &json!({}));
        }
        Ok(db)
    }

    fn console_origin(&self) -> Result<String> {
        Ok(self.env.var("CONSOLE_ORIGIN")?.to_string())
    }

    async fn read_body(
        &self,
        req: &Request,
        path: &str,
    ) -> Result<std::result::Result<Value, ConsoleError>> {
        match req.method() {
            Method::Get => Ok(Ok(json!({}))),
            Method::Post => self.read_post_body(req, path).await,
            _ => Ok(Err(ConsoleError::new(
                STATUS_METHOD_NOT_ALLOWED,
                "method_not_allowed",
            ))),
        }
    }

    async fn read_post_body(
        &self,
        req: &Request,
        path: &str,
    ) -> Result<std::result::Result<Value, ConsoleError>> {
        let expected_origin = self.console_origin()?;
        let origin = req.headers().get("Origin")?;
        let sec_fetch_site = req.headers().get("Sec-Fetch-Site")?;
        let content_type = req.headers().get("Content-Type")?;
        if let Err(error) = check_mutation_headers(
            path,
            &expected_origin,
            origin.as_deref(),
            sec_fetch_site.as_deref(),
            content_type.as_deref(),
        ) {
            return Ok(Err(error));
        }
        if req
            .headers()
            .get("Content-Length")?
            .and_then(|v| v.parse::<usize>().ok())
            .is_some_and(|n| n > MAX_BODY_BYTES)
        {
            return Ok(Err(ConsoleError::new(
                STATUS_PAYLOAD_TOO_LARGE,
                "body_too_large",
            )));
        }
        let bytes = match req.inner().body() {
            None => Vec::new(),
            // Stream with a hard cap instead of req.bytes() so chunked or HTTP/2
            // POSTs without Content-Length cannot buffer unboundedly in Wasm memory.
            Some(stream) => match io::read_bounded_stream(stream, MAX_BODY_BYTES).await {
                Ok(bytes) => bytes,
                Err(io::BodyReadError::TooLarge) => {
                    return Ok(Err(ConsoleError::new(
                        STATUS_PAYLOAD_TOO_LARGE,
                        "body_too_large",
                    )));
                }
                Err(io::BodyReadError::Stream(reason)) => return Err(failure(reason)),
            },
        };
        let Ok(value) = serde_json::from_slice::<Value>(&bytes) else {
            return Ok(Err(ConsoleError::bad_request("invalid_json")));
        };
        if !value.is_object() {
            return Ok(Err(ConsoleError::bad_request("invalid_request")));
        }
        Ok(Ok(value))
    }

    async fn route(&self, mut req: Request) -> Result<Response> {
        let path = req.path();
        if path == "/internal/authorize-turn" && req.method() == Method::Post {
            let digest = req.json().await?;
            // Avoid provisioning schema on uninitialized objects for TURN probes.
            return self.authorize_turn(&Database::new(&self.state), &digest, io::now().0);
        }
        if path == "/internal/authorize" {
            let db = self.database()?;
            return Self::authorize(&db, &req.json().await?, io::now().0);
        }
        if req.url()?.origin().ascii_serialization() != self.console_origin()? {
            return reply_error(ConsoleError::forbidden("console_origin_required"));
        }
        if !path.starts_with("/api/") {
            return reply_error(ConsoleError::not_found());
        }
        let body = match self.read_body(&req, &path).await? {
            Ok(body) => body,
            Err(error) => return reply_error(error),
        };
        let db = self.database()?;
        let now = io::now().0;
        db.cleanup(now)?;
        let response = match (req.method(), path.as_str()) {
            (Method::Post, "/api/login") => self.login(&db, &req, &body, now),
            (Method::Post, p) if is_claimant_endpoint(p) => self.claimant(&db, &req, p, &body, now),
            _ => {
                if !self.authenticated(&db, &req, now)? {
                    return reply_error(ConsoleError::unauthorized("login_required"));
                }
                self.manage(&db, &req, &body, now)
            }
        }?;
        if response.status_code() != 200 && response.status_code() != 201 {
            trace(
                "access_request_refused",
                &json!({
                    "path": path,
                    "status": response.status_code(),
                    "enrollment_id": body.get("enrollment_id"),
                    "credential_id": body.get("credential_id"),
                }),
            );
        }
        self.schedule(&db).await?;
        Ok(response)
    }

    async fn schedule(&self, db: &Database) -> Result<()> {
        let now = io::now().0;
        let cleanup = now.saturating_add(CLEANUP_INTERVAL_MS);
        let deadline = db.next_termination()?.map_or(cleanup, |at| {
            at.max(now.saturating_add(MINIMUM_ALARM_DELAY_MS))
                .min(cleanup)
        });
        let existing = self.state.storage().get_alarm().await?;
        if existing.is_none_or(|at| at > i64::try_from(deadline).unwrap_or(i64::MAX)) {
            self.state
                .storage()
                .set_alarm(io::alarm_time(deadline).map_err(failure)?)
                .await?;
        }
        Ok(())
    }
}

impl DurableObject for AccessObject {
    fn new(state: State, env: Env) -> Self {
        Self {
            state,
            env,
            schema_ready: Cell::new(false),
            turn_limits: RefCell::default(),
        }
    }

    async fn fetch(&self, req: Request) -> Result<Response> {
        match self.route(req).await {
            Ok(response) => Ok(response),
            Err(error) => {
                trace(
                    "access_request_failed",
                    &json!({"reason": error.to_string()}),
                );
                reply_error(ConsoleError::with_retry(
                    STATUS_SERVICE_UNAVAILABLE,
                    "temporarily_unavailable",
                    UNAVAILABLE_RETRY_MS,
                ))
            }
        }
    }

    async fn alarm(&self) -> Result<Response> {
        let db = self.database()?;
        db.cleanup(io::now().0)?;
        self.terminate_shares(&db).await?;
        if db.next_termination()?.is_some() || db.pending_enrollments()? > 0 {
            self.schedule(&db).await?;
        }
        reply(&json!({"ok": true}), 200)
    }
}

pub async fn fetch(env: &Env, req: Request) -> Result<Response> {
    env.durable_object(NAMESPACE)?
        .id_from_name(OBJECT_NAME)?
        .get_stub()?
        .fetch_with_request(req)
        .await
}
