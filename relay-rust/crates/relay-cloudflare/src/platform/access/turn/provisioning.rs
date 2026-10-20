use super::{
    AccessObject, Database, PROVISION_KEY, Provisioning, provider, reply, required, text_id, trace,
};
use serde_json::{Value, json};
use worker::{Response, Result};

impl AccessObject {
    pub(super) fn turn_provision(&self, db: &Database, body: &Value) -> Result<Response> {
        let mut state = db.get::<Provisioning>(PROVISION_KEY)?;
        let service = self.turn_service_id()?;
        let secret_key = provider::secret(&self.env).ok().map(|secret| secret.key_id);
        let action = body.get("action").and_then(Value::as_str);
        match action {
            Some("begin") if state.is_none() => {
                // Ownership is durable before Actions can ask Cloudflare to create a key.
                state = Some(Provisioning::Intent {
                    attempt: text_id(&super::super::random::<16>()?),
                });
            }
            Some("created") => {
                let key = required(body, "key_id")?;
                if key.is_empty()
                    || !key.bytes().all(|byte| byte.is_ascii_alphanumeric())
                    || !state.as_mut().is_some_and(|state| {
                        state.created(body["attempt"].as_str().unwrap_or(""), key)
                    })
                {
                    return reply(&json!({"error":"provisioning_conflict"}), 409);
                }
            }
            Some("publish") => {
                if !state.as_mut().is_some_and(|state| {
                    secret_key.as_deref().is_some_and(|key| {
                        state.publish(body["attempt"].as_str().unwrap_or(""), key)
                    })
                }) {
                    return reply(&json!({"error":"turn_secret_missing"}), 409);
                }
            }
            Some("abandon") => {
                let Some(current) = &state else {
                    return reply(&json!({"error":"provisioning_conflict"}), 409);
                };
                if current.published()
                    || current.attempt() != body["attempt"].as_str().unwrap_or("")
                    || current.key_id() != body["key_id"].as_str()
                    || secret_key.is_some()
                {
                    return reply(&json!({"error":"provisioning_conflict"}), 409);
                }
                state = Some(Provisioning::Abandoning {
                    attempt: current.attempt().to_string(),
                    key_id: required(body, "key_id")?.to_string(),
                });
            }
            Some("replace") => {
                let allowed = state.as_ref().is_some_and(|state| {
                    matches!(state, Provisioning::Abandoning { .. })
                        && state.attempt() == body["attempt"].as_str().unwrap_or("")
                        && state.key_id() == body["key_id"].as_str()
                }) && secret_key.is_none();
                if !allowed {
                    return reply(&json!({"error":"provisioning_conflict"}), 409);
                }
                state = Some(Provisioning::Intent {
                    attempt: text_id(&super::super::random::<16>()?),
                });
            }
            None | Some("begin") => {}
            _ => return reply(&json!({"error":"invalid_request"}), 400),
        }
        if let Some(state) = &state {
            db.put(PROVISION_KEY, state, None)?;
            if action.is_some() {
                trace(
                    "turn_provisioning",
                    &json!({"deployment_id":self.turn_deployment_id()?,"service_id":service,"attempt":state.attempt(),
                    "key_id":state.key_id(),"published":state.published(),"action":action}),
                );
            }
        }
        let key_name = state
            .as_ref()
            .map(|state| format!("windshare:{service}:{}", state.attempt()));
        reply(
            &json!({"service_id":service,"provisioning":state,"key_name":key_name,"secret_key_id":secret_key}),
            200,
        )
    }
}
