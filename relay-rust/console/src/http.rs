//! Transport-independent HTTP error contracts, CSRF/header guards, cookies, and JSON parsing.
use crate::{ADMIN_SESSION_LIFETIME_MS, parse_id};
use serde_json::{Value, json};
use windshare_relay_access::{POLL_INTERVAL_MS, Refusal};

pub const STATUS_ACCEPTED: u16 = 202;
pub const STATUS_BAD_REQUEST: u16 = 400;
pub const STATUS_UNAUTHORIZED: u16 = 401;
pub const STATUS_FORBIDDEN: u16 = 403;
pub const STATUS_NOT_FOUND: u16 = 404;
pub const STATUS_METHOD_NOT_ALLOWED: u16 = 405;
pub const STATUS_CONFLICT: u16 = 409;
pub const STATUS_GONE: u16 = 410;
pub const STATUS_PAYLOAD_TOO_LARGE: u16 = 413;
pub const STATUS_UNSUPPORTED_MEDIA_TYPE: u16 = 415;
pub const STATUS_TOO_MANY_REQUESTS: u16 = 429;
pub const STATUS_SERVICE_UNAVAILABLE: u16 = 503;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ConsoleError {
    pub status: u16,
    pub code: &'static str,
    pub retry_after_ms: u64,
}

impl ConsoleError {
    pub const fn new(status: u16, code: &'static str) -> Self {
        Self {
            status,
            code,
            retry_after_ms: 0,
        }
    }

    pub const fn with_retry(status: u16, code: &'static str, retry_after_ms: u64) -> Self {
        Self {
            status,
            code,
            retry_after_ms,
        }
    }

    pub const fn bad_request(code: &'static str) -> Self {
        Self::new(STATUS_BAD_REQUEST, code)
    }

    pub const fn unauthorized(code: &'static str) -> Self {
        Self::new(STATUS_UNAUTHORIZED, code)
    }

    pub const fn forbidden(code: &'static str) -> Self {
        Self::new(STATUS_FORBIDDEN, code)
    }

    pub const fn not_found() -> Self {
        Self::new(STATUS_NOT_FOUND, "not_found")
    }

    pub const fn too_many_requests(code: &'static str, retry_after_ms: u64) -> Self {
        Self::with_retry(STATUS_TOO_MANY_REQUESTS, code, retry_after_ms)
    }

    pub const fn from_refusal(error: Refusal) -> Self {
        match error {
            Refusal::AccessDenied | Refusal::Cancelled => Self::forbidden("access_denied"),
            Refusal::Expired => Self::new(STATUS_GONE, "expired_token"),
            Refusal::Pending => {
                Self::with_retry(STATUS_ACCEPTED, "authorization_pending", POLL_INTERVAL_MS)
            }
            Refusal::SlowDown { retry_after_ms } => {
                Self::too_many_requests("slow_down", retry_after_ms)
            }
            Refusal::AlreadyCompleted => Self::new(STATUS_CONFLICT, "already_completed"),
            Refusal::InvalidName => Self::bad_request("invalid_name"),
        }
    }

    pub fn body(&self) -> Value {
        if self.retry_after_ms > 0 {
            json!({"error": self.code, "retry_after_ms": self.retry_after_ms})
        } else {
            json!({"error": self.code})
        }
    }
}

impl From<Refusal> for ConsoleError {
    fn from(error: Refusal) -> Self {
        Self::from_refusal(error)
    }
}

impl std::fmt::Display for ConsoleError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.code)
    }
}

impl std::error::Error for ConsoleError {}

pub fn is_claimant_endpoint(path: &str) -> bool {
    matches!(
        path,
        "/api/enrollments"
            | "/api/enrollments/claim"
            | "/api/enrollments/installed"
            | "/api/enrollments/cancel"
    )
}

/// Validates CSRF headers and JSON media type for mutating console API requests.
///
/// # Errors
/// Returns a [`ConsoleError`] with HTTP 403 for cross-site or missing-origin calls
/// and HTTP 415 when the `Content-Type` media type is not `application/json`.
pub fn check_mutation_headers(
    path: &str,
    expected_origin: &str,
    origin: Option<&str>,
    sec_fetch_site: Option<&str>,
    content_type: Option<&str>,
) -> Result<(), ConsoleError> {
    if origin.is_some_and(|value| value != expected_origin) || sec_fetch_site == Some("cross-site")
    {
        return Err(ConsoleError::forbidden("cross_site_request"));
    }
    if !is_claimant_endpoint(path) && origin != Some(expected_origin) {
        return Err(ConsoleError::forbidden("origin_required"));
    }
    let is_json = content_type
        .and_then(|value| value.split(';').next())
        .is_some_and(|media_type| media_type.trim() == "application/json");
    if !is_json {
        return Err(ConsoleError::new(
            STATUS_UNSUPPORTED_MEDIA_TYPE,
            "json_required",
        ));
    }
    Ok(())
}

pub fn extract_cookie<'a>(header: &'a str, name: &str) -> Option<&'a str> {
    header.split(';').find_map(|part| {
        let (cookie_name, value) = part.trim().split_once('=')?;
        (cookie_name == name).then_some(value)
    })
}

pub fn format_session_cookie(name: &str, token: &str, path: &str, secure: bool) -> String {
    let secure_attr = if secure { " Secure;" } else { "" };
    let max_age = if token.is_empty() {
        0
    } else {
        ADMIN_SESSION_LIFETIME_MS / 1_000
    };
    format!(
        "{name}={token}; Path={path}; HttpOnly;{secure_attr} SameSite=Strict; Max-Age={max_age}"
    )
}

/// Extracts a required string field from a JSON request body.
///
/// # Errors
/// Returns `400 invalid_request` when the field is absent or not a JSON string.
pub fn required_str<'a>(body: &'a Value, name: &str) -> Result<&'a str, ConsoleError> {
    body.get(name)
        .and_then(Value::as_str)
        .ok_or_else(|| ConsoleError::bad_request("invalid_request"))
}

/// Extracts and decodes a fixed-width base64url identifier field from a JSON body.
///
/// # Errors
/// Returns `400 invalid_request` when the field is absent, malformed, or the wrong length.
pub fn required_id<const N: usize>(body: &Value, name: &str) -> Result<[u8; N], ConsoleError> {
    parse_id(required_str(body, name)?).ok_or_else(|| ConsoleError::bad_request("invalid_request"))
}

/// Extracts an optional boolean field, defaulting to `false` when omitted.
///
/// # Errors
/// Returns `400 invalid_request` when the field is present with a non-boolean type.
pub fn optional_bool(body: &Value, name: &str) -> Result<bool, ConsoleError> {
    body.get(name).map_or(Ok(false), |value| {
        value
            .as_bool()
            .ok_or_else(|| ConsoleError::bad_request("invalid_request"))
    })
}

#[cfg(test)]
#[path = "http_tests.rs"]
mod tests;
