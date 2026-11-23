use super::{AccessObject, Database, reply, reply_error, store::CloudflareAccessStore, trace};
use serde_json::{Value, json};
use windshare_relay_access::{Digest, Refusal};
use windshare_relay_console::{
    access::{self, AccessError},
    enrollment_response,
    http::{ConsoleError, required_id, required_str},
    text_id,
};
use worker::{Method, Request, Response, Result};

fn finish<T>(
    result: std::result::Result<T, AccessError<worker::Error>>,
    render: impl FnOnce(T) -> Result<Response>,
) -> Result<Response> {
    match result {
        Ok(value) => render(value),
        Err(AccessError::Rejected(error)) => reply_error(error),
        Err(AccessError::Store(error)) => Err(error),
    }
}

impl AccessObject {
    pub(super) fn claimant(
        &self,
        db: &Database,
        req: &Request,
        path: &str,
        body: &Value,
        now: u64,
    ) -> Result<Response> {
        let source_ip = req.headers().get("CF-Connecting-IP")?.unwrap_or_default();
        let mut store = CloudflareAccessStore::new(db, source_ip);
        match path {
            "/api/enrollments" => finish(access::enroll(&mut store, body, now), |outcome| {
                if outcome.created {
                    trace(
                        "enrollment_requested",
                        &json!({
                            "enrollment_id": text_id(&outcome.enrollment.id),
                            "device_name": outcome.enrollment.device_name,
                        }),
                    );
                }
                reply(
                    &enrollment_response(&outcome.enrollment, &self.console_origin()?),
                    200,
                )
            }),
            "/api/enrollments/claim" => finish(access::claim(&mut store, body, now), |outcome| {
                trace(
                    "enrollment_claimed",
                    &json!({
                        "enrollment_id": text_id(&outcome.enrollment_id),
                        "credential_id": text_id(&outcome.credential_id),
                    }),
                );
                reply(&outcome.body, 200)
            }),
            "/api/enrollments/installed" => {
                finish(access::installed(&mut store, body, now), |outcome| {
                    if outcome.newly_installed {
                        trace(
                            "enrollment_installed",
                            &json!({
                                "enrollment_id": text_id(&outcome.enrollment_id),
                                "credential_id": text_id(&outcome.credential_id),
                            }),
                        );
                    }
                    reply(&json!({"ok": true}), 200)
                })
            }
            "/api/enrollments/cancel" => finish(access::cancel(&mut store, body, now), |outcome| {
                if outcome.found {
                    trace(
                        "enrollment_cancelled",
                        &json!({"enrollment_id": text_id(&outcome.enrollment_id)}),
                    );
                }
                reply(&json!({"ok": true}), 200)
            }),
            _ => reply_error(ConsoleError::not_found()),
        }
    }

    pub(super) fn manage(
        &self,
        db: &Database,
        req: &Request,
        body: &Value,
        now: u64,
    ) -> Result<Response> {
        let mut store = CloudflareAccessStore::new(db, String::new());
        match (req.method(), req.path().as_str()) {
            (Method::Get, "/api/session") => reply(&json!({"authenticated": true}), 200),
            (Method::Get, "/api/turn/status") | (Method::Post, "/api/turn/toggle") => {
                Self::turn_setting(db, &req.method(), body)
            }
            (Method::Post, "/api/logout") => Self::logout(db, req),
            (Method::Get, "/api/credentials") => {
                finish(access::list_active_credentials(&store), |value| {
                    reply(&value, 200)
                })
            }
            (Method::Get, "/api/credentials/revoked") => {
                let cursor = req
                    .url()?
                    .query_pairs()
                    .find_map(|(name, value)| (name == "after").then(|| value.into_owned()));
                finish(
                    access::list_revoked_credentials(&store, cursor.as_deref()),
                    |history| {
                        trace(
                            "credential_history_listed",
                            &json!({"continued": history.continued, "has_more": history.has_more}),
                        );
                        reply(&history.body, 200)
                    },
                )
            }
            (Method::Post, "/api/credentials") => {
                let relay = self.env.var("RELAY_URL")?.to_string();
                finish(
                    access::create_service_key(&mut store, body, &relay, now),
                    |created| {
                        trace(
                            "credential_created",
                            &json!({
                                "credential_id": text_id(&created.credential.id),
                                "principal_id": text_id(&created.credential.principal),
                            }),
                        );
                        reply(&created.body, 201)
                    },
                )
            }
            (Method::Post, "/api/credentials/revoke") => finish(
                access::revoke_credential(&mut store, body, now),
                |revoked| {
                    let credential_id = text_id(&revoked.credential.id);
                    if revoked.terminate_shares {
                        db.queue_termination(&credential_id)?;
                    }
                    trace(
                        "credential_revoked",
                        &json!({
                            "credential_id": credential_id,
                            "principal_id": text_id(&revoked.credential.principal),
                            "terminate_shares": revoked.terminate_shares,
                        }),
                    );
                    reply(
                        &json!({"ok": true, "termination_pending": revoked.terminate_shares}),
                        200,
                    )
                },
            ),
            (Method::Get, "/api/enrollments") => {
                finish(access::list_pending_enrollments(&store), |value| {
                    reply(&value, 200)
                })
            }
            (Method::Post, "/api/enrollments/approve") => {
                finish(access::approve(&mut store, body, now), |outcome| {
                    trace(
                        "enrollment_approved",
                        &json!({
                            "enrollment_id": text_id(&outcome.enrollment_id),
                            "principal_id": text_id(&CloudflareAccessStore::owner_principal(db)?),
                        }),
                    );
                    reply(&json!({"ok": true}), 200)
                })
            }
            _ => reply_error(ConsoleError::not_found()),
        }
    }

    pub(super) fn authorize(db: &Database, body: &Value, now: u64) -> Result<Response> {
        let Ok(digest) = required_id::<32>(body, "digest").map(Digest) else {
            return reply_error(ConsoleError::bad_request("invalid_request"));
        };
        let Ok(share) = required_str(body, "share_id") else {
            return reply_error(ConsoleError::bad_request("invalid_request"));
        };
        if windshare_relay_console::parse_id::<12>(share).is_none() {
            return reply_error(ConsoleError::bad_request("invalid_request"));
        }
        let Some(mut credential) = db.credential(digest)? else {
            trace(
                "registration_refused",
                &json!({
                    "share_id": share,
                    "operation_id": body.get("operation_id"),
                    "reason": "unknown_credential",
                }),
            );
            return reply_error(ConsoleError::from_refusal(Refusal::AccessDenied));
        };
        let attribution = match credential.authorize(digest, now) {
            Ok(value) => value,
            Err(error) => {
                trace(
                    "registration_refused",
                    &json!({
                        "share_id": share,
                        "operation_id": body.get("operation_id"),
                        "credential_id": text_id(&credential.id),
                        "principal_id": text_id(&credential.principal),
                        "reason": "revoked_credential",
                    }),
                );
                return reply_error(ConsoleError::from_refusal(error));
            }
        };
        db.save_credential(&credential)?;
        db.track_share(&text_id(&credential.id), share)?;
        trace(
            "registration_authorized",
            &json!({
                "credential_id": text_id(&credential.id),
                "principal_id": text_id(&credential.principal),
                "share_id": share,
                "operation_id": body.get("operation_id"),
            }),
        );
        reply(&json!({"attribution": attribution}), 200)
    }
}
