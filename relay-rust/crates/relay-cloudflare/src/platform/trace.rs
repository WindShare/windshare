use serde_json::json;
use windshare_relay_core::{
    observation::{Observation, Observer, RelayDecision},
    share::Identity,
};

pub struct Trace {
    pub object: String,
    pub identity: Identity,
}

impl Trace {
    pub fn event(&self, event: &str, detail: &serde_json::Value) {
        worker::console_log!(
            "{}",
            &json!({
                "platform": "cloudflare", "event": event, "object": self.object,
                "share": format!("{:?}", self.identity.share),
                "incarnation": format!("{:?}", self.identity.incarnation),
                "at_ms": super::io::now().0, "detail": detail,
            })
        );
    }
}

impl Observer for Trace {
    fn observe(&mut self, observation: Observation) {
        match observation {
            Observation::Lifecycle(value) => self.event(
                "lifecycle_decision",
                &json!({
                    "operation": format!("{:?}", value.operation),
                    "connection": format!("{:?}", value.connection),
                    "cause": format!("{:?}", value.event),
                    "before": format!("{:?}", value.before), "after": format!("{:?}", value.after),
                    "decision": format!("{:?}", value.decision),
                }),
            ),
            Observation::Relay {
                connection,
                decision,
                ..
            } => {
                let mut detail = match decision {
                    RelayDecision::Retired {
                        residual,
                        expires_at,
                    }
                    | RelayDecision::RetirementRiskExpired {
                        residual,
                        expires_at,
                    } => json!({
                        "decision": if matches!(decision, RelayDecision::Retired { .. }) {
                            "risk_retained"
                        } else {
                            "risk_policy_expired"
                        },
                        "risk_expires_at_ms": expires_at.0,
                        "residual_frames": residual.frames,
                        "residual_bytes": residual.encoded_bytes,
                    }),
                    RelayDecision::RetirementBlocked {
                        usage,
                        residual,
                        generation_limit,
                        residual_limit,
                        expires_at,
                    } => json!({
                        "decision": "retirement_capacity_blocked",
                        "risk_expires_at_ms": expires_at.0,
                        "residual_frames": residual.frames, "residual_bytes": residual.encoded_bytes,
                        "retained_generations": usage.retired_generations, "generation_limit": generation_limit,
                        "retained_frames": usage.residual.frames, "frame_limit": residual_limit.frames,
                        "retained_bytes": usage.residual.encoded_bytes, "byte_limit": residual_limit.encoded_bytes,
                    }),
                    _ => json!({"decision": format!("{decision:?}")}),
                };
                detail["connection"] = json!(format!("{connection:?}"));
                self.event("relay_decision", &detail);
            }
            value => self.event(
                "delivery_decision",
                &json!({"decision": format!("{value:?}")}),
            ),
        }
    }
}
