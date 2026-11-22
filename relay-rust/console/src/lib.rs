//! The device console and its transport-independent protocol, HTTP guards, and
//! synchronous enrollment/credential orchestration.
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde_json::{Value, json};
use windshare_relay_access::{Credential, CredentialState, Digest, Enrollment, POLL_INTERVAL_MS};

pub mod access;
pub mod credentials;
pub mod http;

pub const POLICY: &str = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'";
pub const IMMUTABLE_CACHE_CONTROL: &str = "public, max-age=31536000, immutable";
pub const NO_STORE_CACHE_CONTROL: &str = "no-store";
pub const MAX_BODY_BYTES: usize = 4096;
pub const MAX_RECORDS: usize = 1000;
pub const MAX_ACTIVE_CREDENTIALS: usize = 1000;
pub const ADMIN_SESSION_LIFETIME_MS: u64 = 12 * 60 * 60 * 1_000;
const CODE_ALPHABET: &[u8; 32] = b"ABCDEFGHJKLMNPQRSTUVWXYZ23456789";
#[cfg(test)]
#[path = "protocol_tests.rs"]
mod tests;

const KEY_DOMAIN: &[u8] = b"windshare/access-key/v1\0";

pub fn asset(path: &str) -> Option<(&'static str, &'static str)> {
    include!("../console-dist/routes.rs")
}

// Vite content-hashes every emitted file under /assets/; HTML entrypoints and
// unhashed text documents stay uncached so deployments take effect immediately.
pub fn asset_cache_control(path: &str) -> &'static str {
    if path.starts_with("/assets/") {
        IMMUTABLE_CACHE_CONTROL
    } else {
        NO_STORE_CACHE_CONTROL
    }
}

pub fn text_id(bytes: &[u8]) -> String {
    URL_SAFE_NO_PAD.encode(bytes)
}

pub fn parse_id<const N: usize>(text: &str) -> Option<[u8; N]> {
    URL_SAFE_NO_PAD.decode(text).ok()?.try_into().ok()
}

// A claimant can recover the same key after a lost response. Only its digest is persisted.
pub fn claimed_key(secret: &[u8; 32]) -> String {
    format!(
        "wsak_{}",
        text_id(&Digest::of(&[KEY_DOMAIN, secret].concat()).0)
    )
}

pub fn new_access_key(entropy: [u8; 32]) -> String {
    format!("wsak_{}", text_id(&entropy))
}

pub fn user_code(entropy: [u8; 8]) -> String {
    let mut code = String::with_capacity(9);
    for (index, byte) in entropy.into_iter().enumerate() {
        if index == 4 {
            code.push('-');
        }
        code.push(char::from(CODE_ALPHABET[usize::from(byte & 31)]));
    }
    code
}

pub fn enrollment_response(e: &Enrollment, origin: &str) -> Value {
    json!({
        "enrollment_id": text_id(&e.id),
        "user_code": e.user_code,
        "verification_uri": format!("{origin}/"),
        "verification_uri_complete": format!("{origin}/?code={}", e.user_code),
        "expires_at": e.expires_at,
        "interval_ms": POLL_INTERVAL_MS,
    })
}

pub fn pending_enrollment_view(e: &Enrollment) -> Value {
    json!({
        "enrollment_id": text_id(&e.id),
        "user_code": e.user_code,
        "device_name": e.device_name,
        "created_at": e.created_at,
        "expires_at": e.expires_at,
    })
}

pub fn credential_view(c: &Credential) -> Value {
    let revoked_at = match c.state {
        CredentialState::Active => None,
        CredentialState::Revoked { at } => Some(at),
    };
    json!({
        "credential_id": text_id(&c.id),
        "principal_id": text_id(&c.principal),
        "name": c.name,
        "created_at": c.created_at,
        "last_used_at": c.last_used_at,
        "revoked_at": revoked_at,
    })
}

pub fn service_configuration(relay: &str, access_key: &str) -> Value {
    json!({"url": relay, "access_key": access_key})
}
