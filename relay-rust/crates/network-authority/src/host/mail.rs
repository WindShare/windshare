use super::{
    NetworkAuthority, WorkerEnvironment, failure,
    storage::{self, Database},
};
use crate::{
    Authority, EmailVerification, Environment, Fault, Mailbox, Signing,
    challenges::CHALLENGE_SECONDS,
};
use serde::Deserialize;
use worker::{
    AbortSignal, Env, Fetch, Method, Request, RequestInit, RequestRedirect, Response, Result,
    web_sys,
};

const DELIVERY_TIMEOUT_MS: u32 = 15_000;

pub(super) struct Webhook {
    url: url::Url,
    token: String,
}

impl Webhook {
    pub(super) fn configured(env: &Env) -> Option<Self> {
        let url = url::Url::parse(&env.var("EMAIL_WEBHOOK_URL").ok()?.to_string()).ok()?;
        let token = env.secret("EMAIL_WEBHOOK_TOKEN").ok()?.to_string();
        (url.scheme() == "https" && !token.trim().is_empty()).then_some(Self { url, token })
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ProfileEmail {
    email: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RegistrationEmail {
    email: String,
    client_secret: String,
}
fn parse_request(
    bytes: &[u8],
    bearer: Option<String>,
) -> std::result::Result<(Mailbox, EmailVerification), Fault> {
    let (address, verification) = if let Some(bearer) = bearer {
        let input: ProfileEmail = serde_json::from_slice(bytes).map_err(|_| Fault::InvalidEmail)?;
        (input.email, EmailVerification::Profile { bearer })
    } else {
        let input: RegistrationEmail = serde_json::from_slice(bytes).map_err(|_| Fault::Invalid)?;
        (
            input.email,
            EmailVerification::Registration {
                client_secret: input.client_secret,
            },
        )
    };
    Ok((Mailbox::parse(&address)?, verification))
}

fn unavailable() -> Result<Response> {
    super::routes::fault(&Fault::EmailDeliveryUnavailable).map_err(failure)
}
impl NetworkAuthority {
    pub(super) async fn email(
        &self,
        request: &Request,
        bytes: &[u8],
        bearer: Option<String>,
    ) -> Result<Response> {
        if request.method() != Method::Post {
            return Response::error("POST required", 405);
        }
        let (email, verification) = match parse_request(bytes, bearer) {
            Ok(input) => input,
            Err(fault) => return super::routes::fault(&fault).map_err(failure),
        };
        let Some(Webhook { url, token }) = Webhook::configured(&self.env) else {
            return unavailable();
        };
        let recipient = email.as_str().to_owned();
        let profile_change = matches!(&verification, EmailVerification::Profile { .. });
        let source = request
            .headers()
            .get("X-Authority-Source")?
            .unwrap_or_default();
        let mut database = Database::open(&self.state).map_err(failure)?;
        let admission_store = database.clone();
        let signing = Signing::load(
            &self.env.var("AUTHORITY_ROOT")?.to_string(),
            &self.env.var("AUTHORITY_CERTIFICATE")?.to_string(),
            &self.env.secret("AUTHORITY_SIGNING_SEED")?.to_string(),
        )
        .map_err(failure)?;
        let environment = WorkerEnvironment {
            registration_email_configured: true,
        };
        let outcome = storage::transaction(&self.state, move || {
            let mut authority = Authority::new(admission_store, environment, signing)?;
            // Expected admission refusals commit their limiter state.
            match authority.prepare_email(&source, verification, &email) {
                Ok(value) => Ok(Ok(value)),
                Err(error) if error.downcast_ref::<Fault>().is_some() => Ok(Err(error)),
                Err(error) => Err(error),
            }
        })
        .map_err(failure)?;
        let delivery = match outcome {
            Ok(value) => value,
            Err(error) => {
                return error.downcast_ref::<Fault>().map_or_else(
                    || Err(failure(&error)),
                    |fault| super::routes::fault(fault).map_err(failure),
                );
            }
        };
        let body = serde_json::json!({
            "to": recipient,
            "subject": "Your WindShare verification code",
            "text": format!("Verification code: {}\nThis code expires in {} minutes.", delivery.code(), CHALLENGE_SECONDS / 60),
        })
        .to_string();
        let mut request = Request::new_with_init(
            url.as_str(),
            RequestInit::new()
                .with_method(Method::Post)
                // Workers exposes redirects in manual mode so only the configured provider receives the bearer.
                .with_redirect(RequestRedirect::Manual)
                .with_body(Some(body.into())),
        )?;
        request
            .headers_mut()?
            .set("Authorization", &format!("Bearer {token}"))?;
        request
            .headers_mut()?
            .set("Content-Type", "application/json")?;
        let signal = AbortSignal::from(web_sys::AbortSignal::timeout_with_u32(DELIVERY_TIMEOUT_MS));
        let response = match Fetch::Request(request).send_with_signal(&signal).await {
            Ok(response) => response,
            Err(error) => {
                tracing::warn!(
                    event = "email_delivery_failed",
                    challenge_fingerprint = %delivery.challenge_fingerprint(),
                    %error,
                );
                return unavailable();
            }
        };
        if !(200..300).contains(&response.status_code()) {
            tracing::warn!(
                event = "email_delivery_rejected",
                challenge_fingerprint = %delivery.challenge_fingerprint(),
                status = response.status_code()
            );
            return unavailable();
        }
        let challenge = storage::transaction(&self.state, move || {
            delivery.accepted(&mut database, environment.now())
        })
        .map_err(failure)?;
        if profile_change {
            Response::from_json(&serde_json::json!({"address":recipient,"challenge":challenge}))
        } else {
            Response::from_json(&challenge)
        }
    }
}
