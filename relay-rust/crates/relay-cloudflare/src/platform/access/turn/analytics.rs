use super::{AccessObject, Database, failure, io, provider, reply, trace};
use serde::Deserialize;
use serde_json::{Value, json};
use worker::{Env, Fetch, Headers, Method, Request, RequestInit, Response, Result};

const ENDPOINT: &str = "https://api.cloudflare.com/client/v4/graphql";
const DAY_MS: u64 = 24 * 60 * 60 * 1000;
const WINDOW_DAYS: u64 = 7;
const REQUEST_TIMEOUT_MS: u32 = 15_000;
const MAX_RESPONSE_BYTES: usize = 64 * 1024;

#[derive(Deserialize)]
struct AnalyticsSecret {
    account_id: String,
    api_token: String,
}
fn secret(env: &Env) -> Result<AnalyticsSecret> {
    let value: AnalyticsSecret = serde_json::from_str(&env.secret("TURN_ANALYTICS")?.to_string())?;
    if value.account_id.len() != 32
        || !value
            .account_id
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit())
        || value.api_token.is_empty()
    {
        return Err(failure("invalid TURN analytics binding"));
    }
    Ok(value)
}
pub(super) fn available(env: &Env) -> bool {
    secret(env).is_ok()
}
#[expect(
    clippy::cast_precision_loss,
    reason = "These timestamps come from JavaScript Date::now, rounded down to whole days."
)]
fn date(timestamp: u64) -> Result<String> {
    js_sys::Date::new(&wasm_bindgen::JsValue::from_f64(timestamp as f64))
        .to_iso_string()
        .as_string()
        .and_then(|value| value.get(..10).map(str::to_string))
        .ok_or_else(|| failure("invalid analytics date"))
}
impl AccessObject {
    pub(in crate::platform::access) async fn turn_usage(&self, db: &Database) -> Result<Response> {
        if !self.turn_ready(db)? || !available(&self.env) {
            return reply(&json!({"error":"analytics_unavailable"}), 409);
        }
        let result = self.query_turn_usage().await;
        match result {
            Ok(value) => reply(&value, 200),
            Err(error) => {
                trace(
                    "turn_analytics_failed",
                    &json!({"service_id":self.turn_service_id()?,"reason":error.to_string()}),
                );
                reply(&json!({"error":"temporarily_unavailable"}), 503)
            }
        }
    }
    async fn query_turn_usage(&self) -> Result<Value> {
        // Only the explicit owner query uses this separate read-only credential.
        let analytics = secret(&self.env)?;
        let key = provider::secret(&self.env)?.key_id;
        let today = io::now().0 / DAY_MS * DAY_MS;
        let first = date(today.saturating_sub(WINDOW_DAYS * DAY_MS))?;
        let last = date(today.saturating_sub(DAY_MS))?;
        let query = format!(
            "query {{ viewer {{ accounts(filter:{{accountTag:{}}}) {{ callsTurnUsageAdaptiveGroups(limit:1,filter:{{date_geq:{},date_leq:{},keyId:{}}}) {{ sum {{ egressBytes ingressBytes }} }} }} }} }}",
            json!(analytics.account_id),
            json!(first),
            json!(last),
            json!(key)
        );
        let headers = Headers::new();
        headers.set("Authorization", &format!("Bearer {}", analytics.api_token))?;
        headers.set("Content-Type", "application/json")?;
        let request = Request::new_with_init(
            ENDPOINT,
            RequestInit::new()
                .with_method(Method::Post)
                .with_redirect(worker::RequestRedirect::Manual)
                .with_headers(headers)
                .with_body(Some(json!({"query":query}).to_string().into())),
        )?;
        let signal =
            worker::AbortSignal::from(web_sys::AbortSignal::timeout_with_u32(REQUEST_TIMEOUT_MS));
        let mut response = Fetch::Request(request).send_with_signal(&signal).await?;
        if response.status_code() != 200 {
            return Err(failure("TURN analytics request failed"));
        }
        let bytes = response.bytes().await?;
        if bytes.len() > MAX_RESPONSE_BYTES {
            return Err(failure("TURN analytics response exceeds limit"));
        }
        let value: Value = serde_json::from_slice(&bytes)?;
        if value["errors"]
            .as_array()
            .is_some_and(|errors| !errors.is_empty())
        {
            return Err(failure("TURN analytics query failed"));
        }
        let accounts = value["data"]["viewer"]["accounts"]
            .as_array()
            .filter(|accounts| accounts.len() == 1)
            .ok_or_else(|| failure("TURN analytics account unavailable"))?;
        let groups = accounts[0]["callsTurnUsageAdaptiveGroups"]
            .as_array()
            .ok_or_else(|| failure("TURN analytics groups unavailable"))?;
        let mut ingress = 0_u64;
        let mut egress = 0_u64;
        for group in groups {
            ingress = ingress.saturating_add(
                group["sum"]["ingressBytes"]
                    .as_u64()
                    .ok_or_else(|| failure("invalid ingress estimate"))?,
            );
            egress = egress.saturating_add(
                group["sum"]["egressBytes"]
                    .as_u64()
                    .ok_or_else(|| failure("invalid egress estimate"))?,
            );
        }
        Ok(
            json!({"first_date":first,"last_date":last,"ingress_bytes":ingress,"egress_bytes":egress,"sampled":true}),
        )
    }
}
