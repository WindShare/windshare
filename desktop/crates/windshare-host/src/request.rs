use serde::Serialize;

#[derive(Debug, Serialize)]
#[serde(tag = "command", rename_all = "snake_case")]
pub enum Command {
    Create {
        operation_id: String,
        paths: Vec<String>,
    },
    Cancel {
        operation_id: String,
    },
    Stop {
        task_id: String,
    },
}

#[derive(Debug, Serialize)]
pub struct Request<C = Command> {
    pub id: String,
    #[serde(flatten)]
    pub command: C,
}

const MAXIMUM_REQUEST_BYTES: usize = 256 * 1024;

impl Command {
    pub fn validate(&self) -> Result<(), String> {
        // Reserve the longest possible request ID before the worker assigns one.
        // An oversized selection must fail locally, not reconnect forever after
        // the host rejects the WebSocket frame.
        let request = Request {
            id: u64::MAX.to_string(),
            command: self,
        };
        let bytes = serde_json::to_vec(&request).map_err(|error| error.to_string())?;
        if bytes.len() > MAXIMUM_REQUEST_BYTES {
            return Err("Too many selected paths; share their containing folder instead".into());
        }
        Ok(())
    }
}
