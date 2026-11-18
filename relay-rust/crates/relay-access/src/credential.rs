use alloc::string::String;
use serde::{Deserialize, Serialize};
use sha2::{Digest as _, Sha256};

use crate::Attribution;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct Digest(pub [u8; 32]);

impl Digest {
    pub fn of(secret: &[u8]) -> Self {
        Self(Sha256::digest(secret).into())
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub enum CredentialState {
    Active,
    Revoked { at: u64 },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct Credential {
    pub principal: [u8; 16],
    pub id: [u8; 16],
    pub name: String,
    pub digest: Digest,
    pub created_at: u64,
    pub last_used_at: Option<u64>,
    pub state: CredentialState,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Refusal {
    AccessDenied,
    Expired,
    Pending,
    SlowDown { retry_after_ms: u64 },
    Cancelled,
    AlreadyCompleted,
    InvalidName,
}

impl Credential {
    /// Admission is checked for each fresh registration, never cached on a socket.
    ///
    /// # Errors
    /// A revoked or mismatched credential cannot mint a new grant.
    pub fn authorize(&mut self, digest: Digest, now: u64) -> Result<Attribution, Refusal> {
        if self.digest != digest || self.state != CredentialState::Active {
            return Err(Refusal::AccessDenied);
        }
        self.last_used_at = Some(now);
        Ok(Attribution::Credential {
            principal: self.principal,
            credential: self.id,
        })
    }

    pub fn revoke(&mut self, now: u64) {
        if self.state == CredentialState::Active {
            self.state = CredentialState::Revoked { at: now };
        }
    }
}
