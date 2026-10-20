use super::parse_id;
use serde_json::Value;

// Only client-controlled syntax is checked here; storage and host failures keep
// their retryable classification instead of being mistaken for invalid input.
pub(super) fn valid(path: &str, body: &Value) -> bool {
    let Some(fields) = body.as_object() else {
        return false;
    };
    let string = |name: &str| fields.get(name).and_then(Value::as_str);
    let id = |name: &str| string(name).is_some_and(|value| parse_id::<16>(value).is_ok());
    let secret = |name: &str| string(name).is_some_and(|value| parse_id::<32>(value).is_ok());
    match path {
        "/api/login" => string("password").is_some(),
        "/api/enrollments" => string("device_name").is_some() && secret("claim_digest"),
        "/api/enrollments/claim" | "/api/enrollments/installed" | "/api/enrollments/cancel" => {
            id("enrollment_id") && secret("claim_secret")
        }
        "/api/enrollments/approve" => id("enrollment_id") && string("user_code").is_some(),
        "/api/credentials" => string("name").is_some(),
        "/api/credentials/revoke" => {
            id("credential_id") && fields.get("terminate_shares").is_none_or(Value::is_boolean)
        }
        _ => true,
    }
}

pub(super) fn claimant_endpoint(path: &str) -> bool {
    matches!(
        path,
        "/api/enrollments"
            | "/api/enrollments/claim"
            | "/api/enrollments/installed"
            | "/api/enrollments/cancel"
    )
}
