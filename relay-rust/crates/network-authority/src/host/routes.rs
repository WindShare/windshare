use super::{WorkerEnvironment, failure, storage::Database};
use crate::{
    Authority, Block, Fault, Mailbox, NodePolicyChange, PasswordVerifier, Registration,
    ReportPosition, SettingsChanges,
};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::Deserialize;
use worker::{Env, Method, Request, Response};

pub struct Context {
    pub(super) path: String,
    admin_after: Option<String>,
    account_query: Option<String>,
    pub(super) method: Method,
    pub(super) source: String,
    pub(super) bearer: String,
    pub(super) session: String,
    device_session: String,
    pub(super) csrf: String,
    pub(super) initialization: String,
    env: Env,
    extra_email_domains: Vec<String>,
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
        let parameter = |name: &str| -> worker::Result<Option<String>> {
            Ok(request
                .url()?
                .query_pairs()
                .find_map(|(key, value)| (key == name).then(|| value.into_owned())))
        };
        let admin_after = match path.as_str() {
            "/v1/admin/reports" | "/v1/admin/nodes" | "/v1/admin/accounts" => parameter("after")?,
            _ => None,
        };
        let account_query = if path == "/v1/admin/accounts/lookup" {
            parameter("q")?
        } else {
            None
        };
        Ok(Self {
            path,
            admin_after,
            account_query,
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
            env: env.clone(),
            extra_email_domains: super::mail::extra_email_domains(env),
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
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct DismissReview {
    count: u64,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PasswordLoginRequest {
    username: String,
    password_verifier: PasswordVerifier,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AdminInitializeRequest {
    token: String,
    username: String,
    password_verifier: PasswordVerifier,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AdminPasswordChangeRequest {
    current_password_verifier: PasswordVerifier,
    new_password_verifier: PasswordVerifier,
}
pub(super) fn parse<T: serde::de::DeserializeOwned>(bytes: &[u8]) -> Result<T> {
    serde_json::from_slice(bytes).map_err(|_| Fault::Invalid.into())
}
pub fn csrf(session: &str) -> String {
    URL_SAFE_NO_PAD.encode(crate::registration::digest(&format!("csrf/{session}")))
}
pub(super) fn verify_admin_csrf(session: &str, provided: &str) -> Result<()> {
    ensure!(!session.is_empty(), Fault::administrator_session_required());
    ensure!(
        provided == csrf(session),
        Fault::Denied {
            reason: "csrf".into()
        }
    );
    Ok(())
}

pub fn dispatch(
    authority: &mut Authority<Database, WorkerEnvironment>,
    context: Context,
    bytes: &[u8],
) -> Result<Response> {
    if context.path.starts_with("/v1/admin/") {
        return admin_routes(authority, context, bytes);
    }
    api_routes(authority, context, bytes)
}

fn api_routes(
    authority: &mut Authority<Database, WorkerEnvironment>,
    context: Context,
    bytes: &[u8],
) -> Result<Response> {
    let Context {
        method,
        path,
        source,
        bearer,
        device_session,
        extra_email_domains,
        ..
    } = context;
    let value = match (method, path.as_str()) {
        (Method::Post, "/v1/device/preview" | "/v1/device/approve") => {
            return super::device_login::handle(authority, &path, &source, &device_session, bytes);
        }
        (Method::Post, "/v1/accounts") => {
            serde_json::to_value(authority.register(&source, parse::<Registration>(bytes)?)?)?
        }
        (Method::Post, "/v1/accounts/login") => {
            let input: PasswordLoginRequest = parse(bytes)?;
            serde_json::to_value(authority.login(
                &source,
                &input.username,
                &input.password_verifier,
            )?)?
        }
        (Method::Post, "/v1/accounts/password") => {
            let input: PasswordLoginRequest = parse(bytes)?;
            serde_json::to_value(authority.set_password(
                &source,
                &bearer,
                &input.username,
                &input.password_verifier,
            )?)?
        }
        (Method::Post, "/v1/accounts/ticket") => serde_json::to_value(authority.refresh(&bearer)?)?,
        (Method::Post, "/v1/accounts/profile/nickname") => {
            let input: SetNickname = parse(bytes)?;
            serde_json::to_value(authority.set_nickname(&bearer, &input.nickname)?)?
        }
        (Method::Post, "/v1/accounts/profile/email/verify") => {
            let input: ConfirmEmail = parse(bytes)?;
            serde_json::to_value(authority.confirm_email(
                &bearer,
                Mailbox::parse_with_extra_domains(&input.email, &extra_email_domains)?,
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
        admin_after,
        account_query,
        method,
        source,
        session,
        csrf: provided,
        initialization,
        env,
        ..
    } = context;
    if method == Method::Post {
        match path.as_str() {
            "/v1/admin/initialize" => {
                let input: AdminInitializeRequest = parse(bytes)?;
                return signed_in(&authority.initialize_admin(
                    &input.token,
                    &initialization,
                    &input.username,
                    &input.password_verifier,
                )?);
            }
            "/v1/admin/login" => {
                let input: PasswordLoginRequest = parse(bytes)?;
                return signed_in(&authority.admin_login(
                    &source,
                    &input.username,
                    &input.password_verifier,
                )?);
            }
            _ => {}
        }
    }
    if method != Method::Get {
        verify_admin_csrf(&session, &provided)?;
    }
    let value = match (method, path.as_str()) {
        (Method::Post, "/v1/admin/logout") => {
            authority.logout(&session)?;
            return signed_in("");
        }
        (Method::Post, "/v1/admin/password") => {
            let input: AdminPasswordChangeRequest = parse(bytes)?;
            return signed_in(&authority.change_admin_password(
                &source,
                &session,
                &input.current_password_verifier,
                &input.new_password_verifier,
            )?);
        }
        (Method::Get, "/v1/admin/console") => {
            authority.require_admin(&session)?;
            let directory = super::fleet::console(authority, &env)?;
            let mut value = authority.console(&session)?;
            value["csrf"] = csrf(&session).into();
            value["directory"] = directory;
            value
        }
        (Method::Get, "/v1/admin/reports") => {
            let after = admin_after.as_deref().map(report_position).transpose()?;
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
        (Method::Patch, "/v1/admin/settings") => {
            let (settings, mode) =
                authority.update_settings(&session, &parse::<SettingsChanges>(bytes)?)?;
            serde_json::json!({"settings": settings, "registration_mode": mode})
        }
        (Method::Get, "/v1/admin/accounts") => {
            serde_json::to_value(authority.accounts(&session, admin_after.as_deref())?)?
        }
        (Method::Get, "/v1/admin/accounts/lookup") => serde_json::to_value(
            authority.lookup_account(&session, account_query.as_deref().unwrap_or_default())?,
        )?,
        (Method::Get, path) if path.starts_with("/v1/admin/accounts/") => serde_json::to_value(
            authority.account_by_id(&session, &path["/v1/admin/accounts/".len()..])?,
        )?,
        (Method::Get, "/v1/admin/nodes") => {
            let after = admin_after.as_deref().map(node_key).transpose()?;
            serde_json::to_value(authority.nodes(&session, after)?)?
        }
        (Method::Get, path) if path.starts_with("/v1/admin/nodes/") => {
            let key = node_key(&path["/v1/admin/nodes/".len()..])?;
            serde_json::to_value(authority.node_by_key(&session, &key)?)?
        }
        (Method::Put, "/v1/admin/nodes/policy") => {
            let update = parse::<NodePolicyRequest>(bytes)?;
            let node = authority.set_node_policy(&session, &update.key, update.change)?;
            serde_json::to_value(authority.node_details(node)?)?
        }
        (Method::Post, "/v1/admin/blocks") => {
            authority.block(&session, parse::<Block>(bytes)?)?;
            serde_json::to_value(authority.blocks()?)?
        }
        (Method::Post, "/v1/admin/reviews/dismiss") => {
            let input: DismissReview = parse(bytes)?;
            serde_json::to_value(authority.dismiss_review(&session, input.count)?)?
        }
        _ => return Response::error("not found", 404).map_err(|error| anyhow::anyhow!("{error}")),
    };
    Response::from_json(&value).map_err(|error| anyhow::anyhow!("{error}"))
}
fn node_key(text: &str) -> Result<[u8; 32]> {
    if text.len() != 43 {
        return Err(Fault::Invalid.into());
    }
    let key: [u8; 32] = URL_SAFE_NO_PAD
        .decode(text)
        .map_err(|_| Fault::Invalid)?
        .try_into()
        .map_err(|_| Fault::Invalid)?;
    ensure!(key != [0; 32], Fault::Invalid);
    Ok(key)
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

pub(super) fn signed_in(session: &str) -> Result<Response> {
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
        Fault::Conflict { .. } => 409,
        Fault::Denied { .. } => 403,
        Fault::NodeDiscovery { failure } => failure.http_status(),
        Fault::ReportNotFound | Fault::NodeNotFound | Fault::AccountNotFound => 404,
        Fault::RateLimited { .. } | Fault::NicknameCooldown { .. } => 429,
        Fault::Verification { .. } => 428,
        Fault::EmailDeliveryUnavailable => 503,
    };
    Response::from_json(fault)
        .map(|response| response.with_status(status))
        .map_err(|error| anyhow::anyhow!("{error}"))
}
