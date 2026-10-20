use alloc::string::String;
use serde::{Deserialize, Serialize};
use sha2::{Digest as _, Sha256};

/// Stable service ownership, distinct from the sender's cryptographic share identity.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub enum Attribution {
    Anonymous,
    Credential {
        principal: [u8; 16],
        credential: [u8; 16],
    },
}

impl Attribution {
    pub const ENCODED_BYTES: usize = 33;

    #[must_use]
    pub fn encode(self) -> [u8; Self::ENCODED_BYTES] {
        let mut bytes = [0; Self::ENCODED_BYTES];
        if let Self::Credential {
            principal,
            credential,
        } = self
        {
            bytes[0] = 1;
            bytes[1..17].copy_from_slice(&principal);
            bytes[17..].copy_from_slice(&credential);
        }
        bytes
    }

    #[must_use]
    pub fn decode(bytes: [u8; Self::ENCODED_BYTES]) -> Option<Self> {
        match bytes[0] {
            0 if bytes[1..].iter().all(|byte| *byte == 0) => Some(Self::Anonymous),
            1 => {
                let principal = bytes[1..17].try_into().ok()?;
                let credential = bytes[17..].try_into().ok()?;
                (principal != [0; 16] && credential != [0; 16]).then_some(Self::Credential {
                    principal,
                    credential,
                })
            }
            _ => None,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct Digest(pub [u8; 32]);

impl Digest {
    #[must_use]
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
