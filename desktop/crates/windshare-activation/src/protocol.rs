use crate::{IntentId, ShareIntent};
use serde::{Deserialize, Serialize};

// Native path units cost more than UTF-8 on the wire. Host request validation
// separately enforces its 256 KiB JSON envelope before any engine submission.
pub const MAX_MESSAGE_BYTES: usize = 2 * 1024 * 1024;
pub const PRESENTATION_CAPACITY: usize = 32;

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(
    tag = "kind",
    content = "intent",
    rename_all = "snake_case",
    deny_unknown_fields
)]
pub enum Activation {
    ShowWindow,
    ShareSelection(ShareIntent),
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Phase {
    Starting,
    Running,
    Closing,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(tag = "request", rename_all = "snake_case", deny_unknown_fields)]
pub enum Request {
    Probe,
    Activate {
        instance: IntentId,
        activation: Activation,
    },
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(tag = "reply", rename_all = "snake_case", deny_unknown_fields)]
pub enum Reply {
    Status { instance: IntentId, phase: Phase },
    Accepted { instance: IntentId },
    Closing,
    Starting,
    Rejected { message: String },
}

/// # Errors
/// Rejects serialization failures and messages larger than the IPC bound.
pub fn encode<T: Serialize>(value: &T) -> Result<Vec<u8>, String> {
    let bytes = serde_json::to_vec(value).map_err(|error| error.to_string())?;
    if bytes.len() > MAX_MESSAGE_BYTES {
        return Err("Selection is too large; share its containing folder instead".into());
    }
    Ok(bytes)
}

/// # Errors
/// Rejects invalid or oversized activation messages.
pub fn decode<T: serde::de::DeserializeOwned>(bytes: &[u8]) -> Result<T, String> {
    if bytes.len() > MAX_MESSAGE_BYTES {
        return Err("Activation message exceeds the local transport limit".into());
    }
    serde_json::from_slice(bytes).map_err(|error| error.to_string())
}
