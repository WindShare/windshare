use super::{
    WorkerEnvironment,
    routes::{Context, parse, signed_in},
    storage::Database,
};
use crate::{Authority, HashedPassword, PasswordRegistrationPrepare};
use anyhow::Result;
use serde::Deserialize;
use windshare_relay_access::AccountId;
use worker::{Method, Response};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct LoginBegin {
    username: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AccountLoginComplete {
    account: AccountId,
    password_digest: [u8; 32],
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PasswordPrepare {
    username: String,
    password_len: usize,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PasswordCommit {
    username: String,
    password_hash: HashedPassword,
    previous_verifier_digest: Option<[u8; 32]>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct DeviceLoginBegin {
    invitation: String,
    username: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct DeviceLoginComplete {
    invitation: String,
    account: AccountId,
    password_digest: [u8; 32],
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct InitializePrepare {
    token: String,
    username: String,
    password_len: usize,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct InitializeCommit {
    token: String,
    username: String,
    password_hash: HashedPassword,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AdminLoginComplete {
    password_digest: [u8; 32],
}

pub(super) fn dispatch(
    authority: &mut Authority<Database, WorkerEnvironment>,
    context: Context,
    bytes: &[u8],
) -> Result<Response> {
    if context.method != Method::Post {
        return Response::error("not found", 404).map_err(|error| anyhow::anyhow!("{error}"));
    }
    if context.path.starts_with("/internal/admin/") {
        return admin_routes(authority, context, bytes);
    }
    if context.path.starts_with("/internal/device/") {
        return device_routes(authority, context, bytes);
    }
    account_routes(authority, context, bytes)
}

fn account_routes(
    authority: &mut Authority<Database, WorkerEnvironment>,
    context: Context,
    bytes: &[u8],
) -> Result<Response> {
    let Context {
        path,
        source,
        bearer,
        ..
    } = context;
    let value = match path.as_str() {
        "/internal/accounts/register/prepare" => {
            let input: PasswordRegistrationPrepare = parse(bytes)?;
            serde_json::to_value(authority.prepare_password_registration(&source, &input)?)?
        }
        "/internal/accounts/register/commit" => {
            serde_json::to_value(authority.register(&source, parse(bytes)?)?)?
        }
        "/internal/accounts/login/begin" => {
            let input: LoginBegin = parse(bytes)?;
            serde_json::to_value(authority.begin_account_login(&source, &input.username)?)?
        }
        "/internal/accounts/login/complete" => {
            let input: AccountLoginComplete = parse(bytes)?;
            serde_json::to_value(
                authority.complete_account_login(input.account, input.password_digest)?,
            )?
        }
        "/internal/accounts/password/prepare" => {
            let input: PasswordPrepare = parse(bytes)?;
            serde_json::to_value(authority.prepare_password_change(
                &source,
                &bearer,
                &input.username,
                input.password_len,
            )?)?
        }
        "/internal/accounts/password/commit" => {
            let input: PasswordCommit = parse(bytes)?;
            serde_json::to_value(authority.commit_password_change(
                &bearer,
                &input.username,
                input.password_hash,
                input.previous_verifier_digest,
            )?)?
        }
        _ => return Response::error("not found", 404).map_err(|error| anyhow::anyhow!("{error}")),
    };
    Response::from_json(&value).map_err(|error| anyhow::anyhow!("{error}"))
}

fn device_routes(
    authority: &mut Authority<Database, WorkerEnvironment>,
    context: Context,
    bytes: &[u8],
) -> Result<Response> {
    let Context { path, source, .. } = context;
    match path.as_str() {
        "/internal/device/login/begin" => {
            let input: DeviceLoginBegin = parse(bytes)?;
            let value = serde_json::to_value(authority.begin_device_sign_in(
                &source,
                &input.invitation,
                &input.username,
            )?)?;
            Response::from_json(&value).map_err(|error| anyhow::anyhow!("{error}"))
        }
        "/internal/device/login/complete" => {
            let input: DeviceLoginComplete = parse(bytes)?;
            let approved = authority.complete_device_sign_in(
                &input.invitation,
                input.account,
                input.password_digest,
            )?;
            super::device_login::cookie_response(
                &approved.origin,
                &approved.approval,
                &approved.session,
            )
        }
        _ => Response::error("not found", 404).map_err(|error| anyhow::anyhow!("{error}")),
    }
}

fn admin_routes(
    authority: &mut Authority<Database, WorkerEnvironment>,
    context: Context,
    bytes: &[u8],
) -> Result<Response> {
    let Context {
        path,
        source,
        initialization,
        ..
    } = context;
    match path.as_str() {
        "/internal/admin/initialize/prepare" => {
            let input: InitializePrepare = parse(bytes)?;
            authority.prepare_initialization(
                &input.token,
                &initialization,
                &input.username,
                input.password_len,
            )?;
            Response::from_json(&serde_json::json!({})).map_err(|error| anyhow::anyhow!("{error}"))
        }
        "/internal/admin/initialize/commit" => {
            let input: InitializeCommit = parse(bytes)?;
            signed_in(&authority.complete_initialization(
                &input.token,
                &initialization,
                &input.username,
                input.password_hash,
            )?)
        }
        "/internal/admin/login/begin" => {
            let input: LoginBegin = parse(bytes)?;
            let value =
                serde_json::to_value(authority.begin_admin_login(&source, &input.username)?)?;
            Response::from_json(&value).map_err(|error| anyhow::anyhow!("{error}"))
        }
        "/internal/admin/login/complete" => {
            let input: AdminLoginComplete = parse(bytes)?;
            signed_in(&authority.complete_admin_login(input.password_digest)?)
        }
        _ => Response::error("not found", 404).map_err(|error| anyhow::anyhow!("{error}")),
    }
}
