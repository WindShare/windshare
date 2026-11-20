use super::{edge, failure, read_body, routes};
use crate::{
    Fault, HashedPassword, PASSWORD_SALT_BYTES, PasswordChallenge, PasswordChangePreparation,
    PasswordRegistrationPreparation, PasswordRegistrationPrepare, Registration,
    valid_login_password_length, valid_password_length,
};
use serde::{Deserialize, Serialize};
use worker::{Env, Method, Request, RequestInit, Response, Result};

const AUTHORITY_OBJECT: &str = "network-authority-v1";

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct WireRegistration {
    client_secret: String,
    challenge: Option<String>,
    solution: Option<String>,
    username: Option<String>,
    password: Option<String>,
}

#[derive(Deserialize)]
struct LoginRequest {
    username: String,
    password: String,
}

#[derive(Deserialize)]
struct InitializeRequest {
    token: String,
    username: String,
    password: String,
}

#[derive(Deserialize)]
struct DeviceApproveRequest {
    invitation: String,
    login: Option<LoginRequest>,
    #[serde(rename = "username")]
    _username: Option<String>,
}

pub(super) fn handles(method: &Method, path: &str) -> bool {
    *method == Method::Post
        && matches!(
            path,
            "/v1/accounts"
                | "/v1/accounts/login"
                | "/v1/accounts/password"
                | "/v1/device/approve"
                | "/v1/admin/login"
                | "/v1/admin/initialize"
        )
}

pub(super) async fn handle(mut request: Request, env: &Env) -> Result<Response> {
    let path = request.path();
    let source = edge::source_for_request(&request, env)?;
    let authorization = request.headers().get("Authorization")?;
    let cookie = request.headers().get("Cookie")?;
    let Some(bytes) = read_body(&mut request).await? else {
        return Response::error("request too large", 413);
    };
    match path.as_str() {
        "/v1/accounts" => register(env, &source, &bytes).await,
        "/v1/accounts/login" => account_login(env, &source, &bytes).await,
        "/v1/accounts/password" => {
            set_password(env, &source, authorization.as_deref(), &bytes).await
        }
        "/v1/device/approve" => device_approve(env, &source, cookie.as_deref(), &bytes).await,
        "/v1/admin/login" => admin_login(env, &source, &bytes).await,
        "/v1/admin/initialize" => admin_initialize(env, &source, &bytes).await,
        _ => Response::error("not found", 404),
    }
}

async fn register(env: &Env, source: &str, bytes: &[u8]) -> Result<Response> {
    let Ok(wire) = serde_json::from_slice::<WireRegistration>(bytes) else {
        return fault_response(&Fault::Invalid);
    };
    if wire.username.is_none() && wire.password.is_none() {
        return call_do_raw(env, "/v1/accounts", source, None, None, bytes).await;
    }
    let (Some(username), Some(password)) = (wire.username, wire.password) else {
        return fault_response(&Fault::Invalid);
    };
    let prepare = PasswordRegistrationPrepare {
        client_secret: wire.client_secret.clone(),
        challenge: wire.challenge.clone(),
        solution: wire.solution.clone(),
        username: username.clone(),
        password_len: password.len(),
    };
    let mut response = call_do_json(
        env,
        "/internal/accounts/register/prepare",
        source,
        None,
        &prepare,
    )
    .await?;
    if response.status_code() != 200 {
        return Ok(response);
    }
    match response.json::<PasswordRegistrationPreparation>().await? {
        PasswordRegistrationPreparation::Existing { session } => Response::from_json(&session),
        PasswordRegistrationPreparation::Ready => {
            let hashed = match hash_on_edge(&password)? {
                Ok(hashed) => hashed,
                Err(response) => return Ok(response),
            };
            let commit = Registration {
                client_secret: wire.client_secret,
                challenge: wire.challenge,
                solution: wire.solution,
                username: Some(username),
                password: Some(hashed),
            };
            call_do_json(
                env,
                "/internal/accounts/register/commit",
                source,
                None,
                &commit,
            )
            .await
        }
    }
}

async fn account_login(env: &Env, source: &str, bytes: &[u8]) -> Result<Response> {
    let Ok(input) = serde_json::from_slice::<LoginRequest>(bytes) else {
        return fault_response(&Fault::Invalid);
    };
    if !valid_login_password_length(input.password.len()) {
        return credentials_denied();
    }
    let mut begin = call_do_json(
        env,
        "/internal/accounts/login/begin",
        source,
        None,
        &serde_json::json!({ "username": input.username }),
    )
    .await?;
    if begin.status_code() != 200 {
        return Ok(begin);
    }
    let challenge: PasswordChallenge = begin.json().await?;
    if !challenge.password.verify(&input.password) {
        return credentials_denied();
    }
    call_do_json(
        env,
        "/internal/accounts/login/complete",
        source,
        None,
        &serde_json::json!({
            "account": challenge.account,
            "password_digest": challenge.password.fingerprint(),
        }),
    )
    .await
}

async fn set_password(
    env: &Env,
    source: &str,
    authorization: Option<&str>,
    bytes: &[u8],
) -> Result<Response> {
    let Ok(input) = serde_json::from_slice::<LoginRequest>(bytes) else {
        return fault_response(&Fault::Invalid);
    };
    if !valid_password_length(input.password.len()) {
        return fault_response(&Fault::Invalid);
    }
    let mut prepare = call_do_json(
        env,
        "/internal/accounts/password/prepare",
        source,
        authorization,
        &serde_json::json!({
            "username": input.username,
            "password_len": input.password.len(),
        }),
    )
    .await?;
    if prepare.status_code() != 200 {
        return Ok(prepare);
    }
    let prepared: PasswordChangePreparation = prepare.json().await?;
    let hashed = match hash_on_edge(&input.password)? {
        Ok(hashed) => hashed,
        Err(response) => return Ok(response),
    };
    call_do_json(
        env,
        "/internal/accounts/password/commit",
        source,
        authorization,
        &serde_json::json!({
            "username": input.username,
            "password_hash": hashed,
            "previous_verifier_digest": prepared.previous_verifier_digest,
        }),
    )
    .await
}

async fn device_approve(
    env: &Env,
    source: &str,
    cookie: Option<&str>,
    bytes: &[u8],
) -> Result<Response> {
    let Ok(input) = serde_json::from_slice::<DeviceApproveRequest>(bytes) else {
        return fault_response(&Fault::Invalid);
    };
    let Some(login) = input.login else {
        return call_do_raw(env, "/v1/device/approve", source, None, cookie, bytes).await;
    };
    if !valid_login_password_length(login.password.len()) {
        return credentials_denied();
    }
    let mut begin = call_do_json(
        env,
        "/internal/device/login/begin",
        source,
        None,
        &serde_json::json!({
            "invitation": input.invitation,
            "username": login.username,
        }),
    )
    .await?;
    if begin.status_code() != 200 {
        return Ok(begin);
    }
    let challenge: PasswordChallenge = begin.json().await?;
    if !challenge.password.verify(&login.password) {
        return credentials_denied();
    }
    call_do_json(
        env,
        "/internal/device/login/complete",
        source,
        None,
        &serde_json::json!({
            "invitation": input.invitation,
            "account": challenge.account,
            "password_digest": challenge.password.fingerprint(),
        }),
    )
    .await
}

async fn admin_login(env: &Env, source: &str, bytes: &[u8]) -> Result<Response> {
    let Ok(input) = serde_json::from_slice::<LoginRequest>(bytes) else {
        return fault_response(&Fault::Invalid);
    };
    if !valid_login_password_length(input.password.len()) {
        return credentials_denied();
    }
    let mut begin = call_do_json(
        env,
        "/internal/admin/login/begin",
        source,
        None,
        &serde_json::json!({ "username": input.username }),
    )
    .await?;
    if begin.status_code() != 200 {
        return Ok(begin);
    }
    let verifier: HashedPassword = begin.json().await?;
    if !verifier.verify(&input.password) {
        return credentials_denied();
    }
    call_do_json(
        env,
        "/internal/admin/login/complete",
        source,
        None,
        &serde_json::json!({ "password_digest": verifier.fingerprint() }),
    )
    .await
}

async fn admin_initialize(env: &Env, source: &str, bytes: &[u8]) -> Result<Response> {
    let Ok(input) = serde_json::from_slice::<InitializeRequest>(bytes) else {
        return fault_response(&Fault::Invalid);
    };
    let prepare = call_do_json(
        env,
        "/internal/admin/initialize/prepare",
        source,
        None,
        &serde_json::json!({
            "token": input.token,
            "username": input.username,
            "password_len": input.password.len(),
        }),
    )
    .await?;
    if prepare.status_code() != 200 {
        return Ok(prepare);
    }
    let hashed = match hash_on_edge(&input.password)? {
        Ok(hashed) => hashed,
        Err(response) => return Ok(response),
    };
    call_do_json(
        env,
        "/internal/admin/initialize/commit",
        source,
        None,
        &serde_json::json!({
            "token": input.token,
            "username": input.username,
            "password_hash": hashed,
        }),
    )
    .await
}

fn hash_on_edge(password: &str) -> Result<std::result::Result<HashedPassword, Response>> {
    let mut salt = [0_u8; PASSWORD_SALT_BYTES];
    getrandom::fill(&mut salt).map_err(failure)?;
    match HashedPassword::hash(password, &salt) {
        Ok(hashed) => Ok(Ok(hashed)),
        Err(error) => error.downcast_ref::<Fault>().map_or_else(
            || Err(failure(&error)),
            |fault| fault_response(fault).map(Err),
        ),
    }
}

fn credentials_denied() -> Result<Response> {
    fault_response(&crate::password::invalid_credentials())
}

fn fault_response(fault: &Fault) -> Result<Response> {
    routes::fault(fault).map_err(failure)
}

async fn call_do_json(
    env: &Env,
    path: &str,
    source: &str,
    authorization: Option<&str>,
    body: &impl Serialize,
) -> Result<Response> {
    let payload = serde_json::to_vec(body).map_err(failure)?;
    call_do_raw(env, path, source, authorization, None, &payload).await
}

async fn call_do_raw(
    env: &Env,
    path: &str,
    source: &str,
    authorization: Option<&str>,
    cookie: Option<&str>,
    bytes: &[u8],
) -> Result<Response> {
    let url = format!("https://internal{path}");
    let body = js_sys::Uint8Array::from(bytes);
    let mut request = Request::new_with_init(
        &url,
        RequestInit::new()
            .with_method(Method::Post)
            .with_body(Some(body.into())),
    )?;
    let headers = request.headers_mut()?;
    headers.set("Content-Type", "application/json")?;
    headers.set("X-Authority-Source", source)?;
    if let Some(auth) = authorization {
        headers.set("Authorization", auth)?;
    }
    if let Some(cookie) = cookie {
        headers.set("Cookie", cookie)?;
    }
    env.durable_object("AUTHORITY")?
        .id_from_name(AUTHORITY_OBJECT)?
        .get_stub()?
        .fetch_with_request(request)
        .await
}
