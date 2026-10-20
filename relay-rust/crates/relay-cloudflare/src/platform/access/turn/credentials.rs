use super::{
    AccessObject, Credential, CredentialState, Database, LEASE_PREFIX, Lease, LeaseState, enabled,
    io, lease_key, parse_id, provider, reply, save, text_id, trace,
};
use crate::turn::Subject;
use serde::Deserialize;
use serde_json::json;
use worker::{Response, Result};

const MAX_DEVICE_LEASES: usize = 32;
const MAX_DEPLOYMENT_LEASES: usize = 1000;
const RATE_WINDOW_MS: u64 = 60_000;
const DEVICE_ISSUES_PER_MINUTE: u32 = 30;
const DEPLOYMENT_ISSUES_PER_MINUTE: u32 = 300;

#[derive(Deserialize)]
#[expect(
    clippy::struct_field_names,
    reason = "Fields name distinct identifiers in the cross-language wire contract."
)]
#[serde(deny_unknown_fields)]
pub(super) struct LeaseRequest {
    pub service_id: String,
    pub lease_id: String,
    pub session_id: String,
    pub path_id: String,
    pub previous_lease_id: Option<String>,
}
impl LeaseRequest {
    pub fn valid(&self) -> bool {
        [&self.lease_id, &self.session_id, &self.path_id]
            .into_iter()
            .all(|id| parse_id::<16>(id).is_ok_and(|bytes| bytes != [0; 16]))
            && self
                .previous_lease_id
                .as_ref()
                .is_none_or(|id| parse_id::<16>(id).is_ok())
    }
    pub fn subject(&self, credential: &Credential) -> Subject {
        Subject {
            device: text_id(&credential.id),
            session: self.session_id.clone(),
            path: self.path_id.clone(),
        }
    }
}
fn grant_response(lease: &Lease, status: u16) -> Result<Response> {
    let LeaseState::Active { grant } = &lease.state else {
        return reply(&json!({"error":"lease_unavailable"}), 409);
    };
    if lease.usable_until <= io::now().0 {
        return reply(&json!({"error":"lease_unavailable"}), 409);
    }
    reply(
        &json!({"service_id":lease.service_id,"lease_id":lease.id,
        "session_id":lease.subject.session,"path_id":lease.subject.path,
        "urls":grant.urls,"username":grant.username,"password":grant.password,"expires_at":lease.usable_until}),
        status,
    )
}
impl AccessObject {
    pub(super) fn turn_authorized(&self, db: &Database, device: &str) -> Result<bool> {
        Ok(enabled(db)?
            && self.turn_ready(db)?
            && db
                .get::<Credential>(&format!("credential:{device}"))?
                .is_some_and(|value| value.state == CredentialState::Active))
    }
    pub(super) async fn turn_issue(
        &self,
        db: &Database,
        credential: &Credential,
        request: LeaseRequest,
        renewal: bool,
    ) -> Result<Response> {
        let now = io::now().0;
        let subject = request.subject(credential);
        if !self.turn_authorized(db, &subject.device)? {
            return reply(&json!({"error":"access_denied"}), 403);
        }
        if renewal != request.previous_lease_id.is_some() {
            return reply(&json!({"error":"invalid_request"}), 400);
        }
        if let Some(mut lease) = db.get::<Lease>(&lease_key(&request.lease_id))? {
            if !lease.owned_by(&subject) {
                return reply(&json!({"error":"access_denied"}), 403);
            }
            lease.settle_expiry(now);
            return grant_response(&lease, 200);
        }
        if let Some(previous) = &request.previous_lease_id {
            let allowed = db.get::<Lease>(&lease_key(previous))?.is_some_and(|lease| {
                lease.service_id == request.service_id
                    && lease.renewable_by(&subject.device, &subject.session, now)
            });
            if !allowed {
                return reply(&json!({"error":"lease_unavailable"}), 409);
            }
        }
        let leases = db.list::<Lease>(LEASE_PREFIX)?;
        if leases.len() >= MAX_DEPLOYMENT_LEASES
            || leases
                .iter()
                .filter(|lease| {
                    lease.subject.device == subject.device && lease.live() && lease.expires_at > now
                })
                .count()
                >= MAX_DEVICE_LEASES
            || !Self::rate_limit_window(
                db,
                "turn:global",
                DEPLOYMENT_ISSUES_PER_MINUTE,
                now,
                RATE_WINDOW_MS,
            )?
            || !Self::rate_limit_window(
                db,
                &format!("turn:{}", subject.device),
                DEVICE_ISSUES_PER_MINUTE,
                now,
                RATE_WINDOW_MS,
            )?
        {
            return reply(
                &json!({"error":"slow_down","retry_after_ms":RATE_WINDOW_MS}),
                429,
            );
        }
        let secret = provider::secret(&self.env)?;
        let mut lease = Lease::new(
            request.lease_id,
            request.service_id,
            secret.key_id.clone(),
            subject,
            now,
        );
        save(db, &lease)?;
        trace("turn_issuance_started", &context(&lease));
        // Reservation and an alarm survive both process interruption and caller cancellation.
        self.schedule().await?;
        let grant = match provider::issue(&secret).await {
            Ok(grant) => grant,
            Err(error) => {
                lease.state = LeaseState::IssuanceFailed;
                save(db, &lease)?;
                trace(
                    "turn_issuance_failed",
                    &json!({"lease_id":lease.id,"service_id":lease.service_id,
                    "device_id":lease.subject.device,"session_id":lease.subject.session,"reason":error.to_string()}),
                );
                return reply(&json!({"error":"temporarily_unavailable"}), 503);
            }
        };
        if let Some(current) = db.get::<Lease>(&lease_key(&lease.id))? {
            lease = current;
        }
        let authorized = self.turn_authorized(db, &lease.subject.device)?;
        let published = lease.issued(grant, authorized, io::now().0);
        save(db, &lease)?;
        trace(
            if published {
                "turn_credentials_issued"
            } else {
                "turn_issuance_withdrawn"
            },
            &context(&lease),
        );
        if !published {
            self.schedule().await?;
            return reply(&json!({"error":"access_denied"}), 403);
        }
        grant_response(&lease, 201)
    }
}
pub(super) fn context(lease: &Lease) -> serde_json::Value {
    json!({"service_id":lease.service_id,"device_id":lease.subject.device,"session_id":lease.subject.session,
        "path_id":lease.subject.path,"lease_id":lease.id,"expires_at":lease.expires_at,"state":lease.status()})
}
