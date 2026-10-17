use serde_json::json;
use windshare_relay_core::{
    observation::{Observation, Observer},
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
            } => self.event(
                "relay_decision",
                &json!({
                    "connection": format!("{connection:?}"), "decision": format!("{decision:?}"),
                }),
            ),
            value => self.event(
                "delivery_decision",
                &json!({"decision": format!("{value:?}")}),
            ),
        }
    }
}
