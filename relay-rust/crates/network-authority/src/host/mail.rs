use super::{
    Clock, NetworkAuthority, failure,
    storage::{self, Database},
};
use crate::{Authority, Fault, Signing};
use futures_util::future::{Either, select};
use serde::Deserialize;
use std::time::Duration;
use worker::{
    AbortController, Delay, Fetch, Method, Request, RequestInit, RequestRedirect, Response, Result,
};

const DELIVERY_TIMEOUT: Duration = Duration::from_secs(15);

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Email {
    email: String,
}
impl NetworkAuthority {
    pub(super) async fn email(&self, request: &Request, bytes: &[u8]) -> Result<Response> {
        if request.method() != Method::Post {
            return Response::error("POST required", 405);
        }
        let email: Email = serde_json::from_slice(bytes).map_err(failure)?;
        if email.email.len() > 254
            || !email.email.contains('@')
            || email.email.contains(['\r', '\n'])
        {
            return Response::error("invalid email", 400);
        }
        let url = self.env.var("EMAIL_WEBHOOK_URL")?.to_string();
        if !url.starts_with("https://") {
            return Response::error("email delivery unavailable", 503);
        }
        let token = self.env.secret("EMAIL_WEBHOOK_TOKEN")?.to_string();
        let source = request
            .headers()
            .get("X-Authority-Source")?
            .unwrap_or_default();
        let database = Database::open(&self.state).map_err(failure)?;
        let signing = Signing::load(
            &self.env.var("AUTHORITY_ROOT")?.to_string(),
            &self.env.var("AUTHORITY_CERTIFICATE")?.to_string(),
            &self.env.secret("AUTHORITY_SIGNING_SEED")?.to_string(),
        )
        .map_err(failure)?;
        let outcome = storage::transaction(&self.state, move || {
            let mut authority = Authority::new(database, Clock, signing)?;
            // Expected admission refusals commit their limiter state.
            match authority.email_challenge(&source) {
                Ok(value) => Ok(Ok(value)),
                Err(error) if error.downcast_ref::<Fault>().is_some() => Ok(Err(error)),
                Err(error) => Err(error),
            }
        })
        .map_err(failure)?;
        let (challenge, secret) = match outcome {
            Ok(value) => value,
            Err(error) => {
                return error.downcast_ref::<Fault>().map_or_else(
                    || Err(failure(&error)),
                    |fault| super::routes::fault(fault).map_err(failure),
                );
            }
        };
        let body =
            serde_json::json!({"to":email.email,"subject":"Your WindShare verification code",
            "text":format!("Verification code: {secret}\nThis code expires in 15 minutes.")})
            .to_string();
        let mut request = Request::new_with_init(
            &url,
            RequestInit::new()
                .with_method(Method::Post)
                .with_redirect(RequestRedirect::Error)
                .with_body(Some(body.into())),
        )?;
        request
            .headers_mut()?
            .set("Authorization", &format!("Bearer {token}"))?;
        request
            .headers_mut()?
            .set("Content-Type", "application/json")?;
        let controller = AbortController::default();
        let signal = controller.signal();
        let fetch = Fetch::Request(request);
        let response = match select(
            Box::pin(fetch.send_with_signal(&signal)),
            Box::pin(Delay::from(DELIVERY_TIMEOUT)),
        )
        .await
        {
            Either::Left((response, _)) => response?,
            Either::Right(((), _)) => {
                controller.abort();
                return Response::error("email delivery unavailable", 503);
            }
        };
        if !(200..300).contains(&response.status_code()) {
            return Response::error("email delivery unavailable", 503);
        }
        Response::from_json(&challenge)
    }
}
