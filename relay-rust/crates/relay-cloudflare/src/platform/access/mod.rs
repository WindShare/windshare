mod enrollment;
mod management;
mod request;
mod sessions;
mod storage;
mod termination;

use super::io;
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde_json::{Value, json};
use storage::Database;
use windshare_relay_access::{Digest, Refusal};
use worker::{DurableObject, Env, Method, Request, Response, Result, State, durable_object};

pub const NAMESPACE: &str = "ACCESS";
pub const OBJECT_NAME: &str = "authority";
pub const CREDENTIAL_HEADER: &str = "x-windshare-credential-digest";
pub const CREDENTIAL_TAG: &str = "credential:";
const MAX_BODY_BYTES: usize = 4096;
const MAX_RECORDS: usize = 1000;
const CLEANUP_INTERVAL_MS: u64 = 60_000;
const MINIMUM_ALARM_DELAY_MS: u64 = 1_000;
const COOKIE_NAME: &str = "__Host-windshare_admin";

#[durable_object]
pub struct AccessObject {
    state: State,
    env: Env,
}

pub fn failure(error: impl std::fmt::Display) -> worker::Error {
    worker::Error::RustError(error.to_string())
}
pub fn text_id(bytes: &[u8]) -> String {
    URL_SAFE_NO_PAD.encode(bytes)
}
pub fn parse_id<const N: usize>(text: &str) -> Result<[u8; N]> {
    URL_SAFE_NO_PAD
        .decode(text)
        .map_err(failure)?
        .try_into()
        .map_err(|_| failure("invalid identifier"))
}
fn random<const N: usize>() -> Result<[u8; N]> {
    io::random().map_err(failure)
}
fn key(kind: &str, id: &[u8]) -> String {
    format!("{kind}:{}", text_id(id))
}
fn required<'a>(body: &'a Value, name: &str) -> Result<&'a str> {
    body.get(name)
        .and_then(Value::as_str)
        .ok_or_else(|| failure(format!("missing {name}")))
}
fn trace(event: &str, fields: &Value) {
    worker::console_log!(
        "{}",
        json!({"platform":"cloudflare","event":event,"context":fields})
    );
}
fn reply(value: &Value, status: u16) -> Result<Response> {
    let mut response = Response::from_json(value)?.with_status(status);
    response.headers_mut().set("Cache-Control", "no-store")?;
    response
        .headers_mut()
        .set("X-Content-Type-Options", "nosniff")?;
    Ok(response)
}
fn refusal(error: Refusal) -> Result<Response> {
    let (code, status, retry) = match error {
        Refusal::AccessDenied | Refusal::Cancelled => ("access_denied", 403, 0),
        Refusal::Expired => ("expired_token", 410, 0),
        Refusal::Pending => (
            "authorization_pending",
            202,
            windshare_relay_access::POLL_INTERVAL_MS,
        ),
        Refusal::SlowDown { retry_after_ms } => ("slow_down", 429, retry_after_ms),
        Refusal::AlreadyCompleted => ("already_completed", 409, 0),
        Refusal::InvalidName => ("invalid_name", 400, 0),
    };
    reply(&json!({"error":code,"retry_after_ms":retry}), status)
}

impl AccessObject {
    fn console_origin(&self) -> Result<String> {
        Ok(self.env.var("CONSOLE_ORIGIN")?.to_string())
    }

    async fn route(&self, mut req: Request) -> Result<Response> {
        let db = Database::open(&self.state)?;
        let now = io::now().0;
        let path = req.path();
        if path == "/internal/authorize" {
            return Self::authorize(&db, &req.json().await?, now);
        }
        if req.url()?.origin().ascii_serialization() != self.console_origin()? {
            return reply(&json!({"error":"console_origin_required"}), 403);
        }
        if req.method() == Method::Get && !path.starts_with("/api/") {
            return Self::asset(&path);
        }
        if req.method() != Method::Get {
            if req
                .headers()
                .get("Origin")?
                .is_some_and(|origin| Some(origin) != self.console_origin().ok())
                || req
                    .headers()
                    .get("Sec-Fetch-Site")?
                    .is_some_and(|site| site == "cross-site")
            {
                return reply(&json!({"error":"cross_site_request"}), 403);
            }
            if !request::claimant_endpoint(&path)
                && req.headers().get("Origin")?.as_deref() != Some(self.console_origin()?.as_str())
            {
                return reply(&json!({"error":"origin_required"}), 403);
            }
            if !req.headers().get("Content-Type")?.is_some_and(|value| {
                value
                    .split(';')
                    .next()
                    .is_some_and(|kind| kind.trim() == "application/json")
            }) {
                return reply(&json!({"error":"json_required"}), 415);
            }
        }
        let body = if req.method() == Method::Get {
            json!({})
        } else {
            if req
                .headers()
                .get("Content-Length")?
                .and_then(|v| v.parse::<usize>().ok())
                .is_some_and(|n| n > MAX_BODY_BYTES)
            {
                return reply(&json!({"error":"body_too_large"}), 413);
            }
            let bytes = req.bytes().await?;
            if bytes.len() > MAX_BODY_BYTES {
                return reply(&json!({"error":"body_too_large"}), 413);
            }
            match serde_json::from_slice(&bytes) {
                Ok(value) => value,
                Err(_) => return reply(&json!({"error":"invalid_json"}), 400),
            }
        };
        if req.method() == Method::Post && !request::valid(&path, &body) {
            trace(
                "access_request_refused",
                &json!({"path":path,"reason":"invalid_request"}),
            );
            return reply(&json!({"error":"invalid_request"}), 400);
        }
        db.cleanup(now)?;
        let response = match (req.method(), path.as_str()) {
            (Method::Post, "/api/login") => self.login(&db, &req, &body, now),
            (Method::Post, "/api/enrollments") => self.enroll(&db, &req, &body, now),
            (Method::Post, "/api/enrollments/claim") => Self::claim(&db, &body, now),
            (Method::Post, "/api/enrollments/installed") => Self::installed(&db, &body, now),
            (Method::Post, "/api/enrollments/cancel") => Self::cancel(&db, &body, now),
            _ => {
                if !self.authenticated(&db, &req, now)? {
                    return reply(&json!({"error":"login_required"}), 401);
                }
                self.manage(&db, &req, &body, now)
            }
        }?;
        if response.status_code() != 200 && response.status_code() != 201 {
            trace(
                "access_request_refused",
                &json!({"path":path,"status":response.status_code(),
                "enrollment_id":body.get("enrollment_id"),"credential_id":body.get("credential_id")}),
            );
        }
        self.schedule().await?;
        Ok(response)
    }

    async fn schedule(&self) -> Result<()> {
        let now = io::now().0;
        let cleanup = now.saturating_add(CLEANUP_INTERVAL_MS);
        let deadline = Database::open(&self.state)?
            .next_termination()?
            .map_or(cleanup, |at| {
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

    fn asset(path: &str) -> Result<Response> {
        let (content, kind) = match path {
            "/" => (
                include_str!(concat!(
                    env!("CARGO_MANIFEST_DIR"),
                    "/../../console/index.html"
                )),
                "text/html; charset=utf-8",
            ),
            "/console.js" => (
                include_str!(concat!(
                    env!("CARGO_MANIFEST_DIR"),
                    "/../../console/console.js"
                )),
                "text/javascript; charset=utf-8",
            ),
            "/console.css" => (
                include_str!(concat!(
                    env!("CARGO_MANIFEST_DIR"),
                    "/../../console/console.css"
                )),
                "text/css; charset=utf-8",
            ),
            _ => return Response::error("not found", 404),
        };
        let mut response = Response::ok(content)?;
        for (name, value) in [
            ("Content-Type", kind),
            ("Cache-Control", "no-store"),
            ("X-Content-Type-Options", "nosniff"),
            (
                "Content-Security-Policy",
                "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'",
            ),
            ("Referrer-Policy", "no-referrer"),
            ("Cross-Origin-Opener-Policy", "same-origin"),
        ] {
            response.headers_mut().set(name, value)?;
        }
        Ok(response)
    }
}

impl DurableObject for AccessObject {
    fn new(state: State, env: Env) -> Self {
        Self { state, env }
    }
    async fn fetch(&self, req: Request) -> Result<Response> {
        match self.route(req).await {
            Ok(response) => Ok(response),
            Err(error) => {
                trace(
                    "access_request_failed",
                    &json!({"reason":error.to_string()}),
                );
                reply(
                    &json!({"error":"temporarily_unavailable","retry_after_ms":5_000}),
                    503,
                )
            }
        }
    }
    async fn alarm(&self) -> Result<Response> {
        let db = Database::open(&self.state)?;
        db.cleanup(io::now().0)?;
        self.terminate_shares(&db).await?;
        if db.next_termination()?.is_some()
            || db
                .list::<windshare_relay_access::Enrollment>("enrollment:")?
                .iter()
                .any(|e| {
                    !matches!(
                        e.state,
                        windshare_relay_access::EnrollmentState::Installed { .. }
                    )
                })
        {
            self.schedule().await?;
        }
        reply(&json!({"ok":true}), 200)
    }
}

pub async fn fetch(env: &Env, req: Request) -> Result<Response> {
    env.durable_object(NAMESPACE)?
        .id_from_name(OBJECT_NAME)?
        .get_stub()?
        .fetch_with_request(req)
        .await
}
