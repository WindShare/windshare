use crate::{credential_view, text_id};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde_json::{Value, json};
use windshare_relay_access::{Credential, CredentialState};

pub const CREDENTIAL_HISTORY_PAGE_SIZE: usize = 50;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct RevokedCredentialPosition {
    revoked_at: u64,
    credential_id: [u8; 16],
}

impl RevokedCredentialPosition {
    const fn from_credential(credential: &Credential) -> Option<Self> {
        let CredentialState::Revoked { at } = credential.state else {
            return None;
        };
        Some(Self {
            revoked_at: at,
            credential_id: credential.id,
        })
    }

    pub fn parse(value: &str) -> Option<Self> {
        let (time, id) = value.split_once('.')?;
        let revoked_at = u64::try_from(time.parse::<i64>().ok()?).ok()?;
        if id.len() != 22 {
            return None;
        }
        let credential_id = URL_SAFE_NO_PAD.decode(id).ok()?.try_into().ok()?;
        Some(Self {
            revoked_at,
            credential_id,
        })
    }

    pub const fn revoked_at(&self) -> u64 {
        self.revoked_at
    }

    pub const fn credential_id(&self) -> [u8; 16] {
        self.credential_id
    }

    fn cursor(&self) -> String {
        format!("{}.{}", self.revoked_at, text_id(&self.credential_id))
    }
}

pub fn credential_history(mut credentials: Vec<Credential>) -> Value {
    let has_more = credentials.len() > CREDENTIAL_HISTORY_PAGE_SIZE;
    credentials.truncate(CREDENTIAL_HISTORY_PAGE_SIZE);
    let next = credentials
        .last()
        .filter(|_| has_more)
        .and_then(RevokedCredentialPosition::from_credential)
        .map(|position| position.cursor());
    let credentials: Vec<_> = credentials.iter().map(credential_view).collect();
    json!({"credentials": credentials, "next_cursor": next})
}
