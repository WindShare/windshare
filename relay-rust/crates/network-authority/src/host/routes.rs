use super::{WorkerEnvironment, failure, storage::Database};
use crate::{
    Authority, Block, Fault, Mailbox, NodeEnrollment, NodePolicyChange, ReportPosition, Settings,
};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::Deserialize;
use worker::{Env, Method, Request, Response};

pub struct Context {
    path: String,
    report_after: Option<String>,
    method: Method,
    source: String,
    bearer: String,
    session: String,
    device_session: String,
    csrf: String,
    initialization: String,
    directory: Option<serde_json::Value>,
}
impl Context {
    pub fn new(request: &Request, env: &Env) -> worker::Result<Self> {
        let cookie = request.headers().get("Cookie")?.unwrap_or_default();
        let session = cookie
            .split(';')
            .find_map(|part| part.trim().strip_prefix("authority_session="))
            .unwrap_or_default()
            .to_owned();
        let path = request.path();
        let report_after = if path == "/v1/admin/reports" {
            request
                .url()?
                .query_pairs()
                .find_map(|(name, value)| (name == "after").then(|| value.into_owned()))
        } else {
            None
        };
        Ok(Self {
            path,
            report_after,
            method: request.method(),
            source: request
                .headers()
                .get("X-Authority-Source")?
                .unwrap_or_default(),
            bearer: bearer(request)?,
            session,
            device_session: cookie
                .split(';')
                .find_map(|part| part.trim().strip_prefix(super::device_login::COOKIE))
                .unwrap_or_default()
                .to_owned(),
            csrf: request.headers().get("X-CSRF-Token")?.unwrap_or_default(),
            initialization: env
                .secret("INITIALIZATION_TOKEN")
                .map_or_else(|_| String::new(), |value| value.to_string()),
            directory: env
                .var("DIRECTORY_PUBLICATION")
                .ok()
                .and_then(|v| serde_json::from_str(&v.to_string()).ok()),
        })
    }
}
pub(super) fn bearer(request: &Request) -> worker::Result<String> {
    Ok(request
        .headers()
        .get("Authorization")?
        .and_then(|value| value.strip_prefix("Bearer ").map(str::to_owned))
        .unwrap_or_default())
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct SetNickname {
    nickname: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ConfirmEmail {
    email: String,
    challenge: String,
    code: String,
}
#[derive(Deserialize)]
struct Login {
    username: String,
    password: String,
}
#[derive(Deserialize)]
struct Initialize {
    token: String,
    username: String,
    password: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct NodePolicyRequest {
    key: [u8; 32],
    change: NodePolicyChange,
}
#[derive(Deserialize)]
struct Revoke {
    credential: [u8; 16],
}
#[derive(Deserialize)]
struct Report {
    share: [u8; 12],
    reason: String,
}
fn parse<T: serde::de::DeserializeOwned>(bytes: &[u8]) -> Result<T> {
    serde_json::from_slice(bytes).map_err(|_| Fault::Invalid.into())
}
pub fn csrf(session: &str) -> String {
    URL_SAFE_NO_PAD.encode(crate::registration::digest(&format!("csrf/{session}")))
}

pub fn dispatch(
    authority: &mut Authority<Database, WorkerEnvironment>,
    context: Context,
    bytes: &[u8],
) -> Result<Response> {
    if context.path.starts_with("/v1/admin/") {
        return admin_routes(authority, context, bytes);
    }
    let Context {
        method,
        path,
        source,
        bearer,
        device_session,
        ..
    } = context;
    let value = match (method, path.as_str()) {
        (Method::Post, "/v1/device/preview" | "/v1/device/approve") => {
            return super::device_login::handle(authority, &path, &source, &device_session, bytes);
        }
        (Method::Post, "/v1/accounts") => {
            serde_json::to_value(authority.register(&source, parse(bytes)?)?)?
        }
        (Method::Post, "/v1/accounts/ticket") => serde_json::to_value(authority.refresh(&bearer)?)?,
        (Method::Post, "/v1/accounts/login") => {
            authority.throttle(&source, "account-login", 10, 60)?;
            let login: Login = parse(bytes)?;
            serde_json::to_value(authority.login(&login.username, &login.password)?)?
        }
        (Method::Post, "/v1/accounts/password") => {
            let login: Login = parse(bytes)?;
            serde_json::to_value(authority.set_password(
                &bearer,
                &login.username,
                &login.password,
            )?)?
        }
        (Method::Post, "/v1/accounts/profile/nickname") => {
            let input: SetNickname = parse(bytes)?;
            serde_json::to_value(authority.set_nickname(&bearer, &input.nickname)?)?
        }
        (Method::Post, "/v1/accounts/profile/email/verify") => {
            let input: ConfirmEmail = parse(bytes)?;
            serde_json::to_value(authority.confirm_email(
                &bearer,
                Mailbox::parse(&input.email)?,
                &input.challenge,
                &input.code,
            )?)?
        }
        (Method::Post, "/v1/accounts/credentials/revoke") => {
            let value: Revoke = parse(bytes)?;
            authority.revoke_credential(&bearer, value.credential)?;
            serde_json::json!({})
        }
        (Method::Post, "/v1/node/usage") => serde_json::to_value(authority.usage(bytes)?)?,
        (Method::Post, "/v1/node/restrictions") => {
            serde_json::to_value(authority.restriction_page(bytes)?)?
        }
        (Method::Get, "/v1/blocks") => authority.public_blocks()?,
        (Method::Post, "/v1/reports") => {
            let report: Report = parse(bytes)?;
            serde_json::json!({"id":authority.report(&source,report.share,report.reason)?})
        }
        _ => return Response::error("not found", 404).map_err(|error| anyhow::anyhow!("{error}")),
    };
    Response::from_json(&value).map_err(|error| anyhow::anyhow!("{error}"))
}

fn admin_routes(
    authority: &mut Authority<Database, WorkerEnvironment>,
    context: Context,
    bytes: &[u8],
) -> Result<Response> {
    let Context {
        path,
        report_after,
        method,
        source,
        session,
        csrf: provided,
        initialization,
        directory,
        ..
    } = context;
    if method != Method::Get && !matches!(path.as_str(), "/v1/admin/initialize" | "/v1/admin/login")
    {
        ensure!(!session.is_empty(), Fault::administrator_session_required());
        ensure!(
            provided == csrf(&session),
            Fault::Denied {
                reason: "csrf".into()
            }
        );
    }
    let value = match (method, path.as_str()) {
        (Method::Post, "/v1/admin/initialize") => {
            let value: Initialize = parse(bytes)?;
            return signed_in(&authority.initialize(
                &value.token,
                &initialization,
                &value.username,
                &value.password,
            )?);
        }
        (Method::Post, "/v1/admin/login") => {
            let value: Login = parse(bytes)?;
            return signed_in(&authority.admin_login(&source, &value.username, &value.password)?);
        }
        (Method::Post, "/v1/admin/logout") => {
            authority.logout(&session)?;
            return signed_in("");
        }
        (Method::Get, "/v1/admin/console") => {
            let mut value = authority.console(&session)?;
            value["csrf"] = csrf(&session).into();
            value["directory"] = directory.into();
            value
        }
        (Method::Get, "/v1/admin/reports") => {
            let after = report_after.as_deref().map(report_position).transpose()?;
            let page = authority.reports(&session, after)?;
            let next = page.next.map(|position| {
                format!(
                    "{}.{}",
                    position.created_at,
                    URL_SAFE_NO_PAD.encode(position.id)
                )
            });
            serde_json::json!({ "reports": page.reports, "next_cursor": next })
        }
        (Method::Get, path) if path.starts_with("/v1/admin/reports/") => {
            let id = report_id(&path["/v1/admin/reports/".len()..])?;
            serde_json::to_value(authority.report_by_id(&session, id)?)?
        }
        (Method::Put, "/v1/admin/settings") => {
            let mode = authority.set_settings(&session, &parse::<Settings>(bytes)?)?;
            serde_json::json!({"registration_mode": mode})
        }
        (Method::Post, "/v1/admin/nodes") => serde_json::to_value(
            authority.enroll_node(&session, &parse::<NodeEnrollment>(bytes)?)?,
        )?,
        (Method::Put, "/v1/admin/nodes/policy") => {
            let update = parse::<NodePolicyRequest>(bytes)?;
            serde_json::to_value(authority.set_node_policy(
                &session,
                &update.key,
                update.change,
            )?)?
        }
        (Method::Post, "/v1/admin/blocks") => {
            authority.block(&session, parse::<Block>(bytes)?)?;
            serde_json::to_value(authority.blocks()?)?
        }
        _ => return Response::error("not found", 404).map_err(|error| anyhow::anyhow!("{error}")),
    };
    Response::from_json(&value).map_err(|error| anyhow::anyhow!("{error}"))
}
fn report_id(text: &str) -> Result<[u8; 16]> {
    if text.len() != 22 {
        return Err(Fault::Invalid.into());
    }
    URL_SAFE_NO_PAD
        .decode(text)
        .map_err(|_| Fault::Invalid)?
        .try_into()
        .map_err(|_| Fault::Invalid.into())
}

fn report_position(text: &str) -> Result<ReportPosition> {
    let (time, id) = text.split_once('.').ok_or(Fault::Invalid)?;
    Ok(ReportPosition {
        created_at: time
            .parse::<i64>()
            .ok()
            .and_then(|time| u64::try_from(time).ok())
            .ok_or(Fault::Invalid)?,
        id: report_id(id)?,
    })
}

fn signed_in(session: &str) -> Result<Response> {
    let mut response =
        Response::from_json(&serde_json::json!({"csrf":csrf(session)})).map_err(failure)?;
    response.headers_mut().set("Set-Cookie",&format!("authority_session={session}; HttpOnly; Secure; SameSite=Strict; Path=/v1/admin; Max-Age={}",
        if session.is_empty(){0}else{43200})).map_err(failure)?;
    Ok(response)
}
pub fn fault(fault: &Fault) -> Result<Response> {
    let status = match fault {
        Fault::Invalid
        | Fault::InvalidNickname
        | Fault::InvalidEmail
        | Fault::UnsupportedEmail
        | Fault::EmailCodeRejected => 400,
        Fault::Denied { .. } => 403,
        Fault::ReportNotFound => 404,
        Fault::RateLimited { .. } | Fault::NicknameCooldown { .. } => 429,
        Fault::Verification { .. } => 428,
        Fault::EmailDeliveryUnavailable => 503,
    };
    Response::from_json(fault)
        .map(|response| response.with_status(status))
        .map_err(|error| anyhow::anyhow!("{error}"))
}
