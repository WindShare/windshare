use crate::{Digest, Refusal};
use alloc::string::String;
use serde::{Deserialize, Serialize};

pub const ENROLLMENT_LIFETIME_MS: u64 = 15 * 60 * 1_000;
pub const POLL_INTERVAL_MS: u64 = 5_000;
const MAX_NAME_BYTES: usize = 128;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub enum EnrollmentState {
    Pending,
    Approved { credential: [u8; 16] },
    Claimed { credential: [u8; 16] },
    Installed { credential: [u8; 16] },
    Cancelled,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct Enrollment {
    pub id: [u8; 16],
    pub user_code: String,
    pub device_name: String,
    pub claim_digest: Digest,
    pub created_at: u64,
    pub expires_at: u64,
    pub next_poll_at: u64,
    pub state: EnrollmentState,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Poll {
    Pending,
    Approved { credential: [u8; 16] },
    Installed { credential: [u8; 16] },
}

impl Enrollment {
    /// # Errors
    /// Names must be short display labels without control characters.
    pub fn new(
        id: [u8; 16],
        user_code: String,
        device_name: String,
        claim_digest: Digest,
        now: u64,
    ) -> Result<Self, Refusal> {
        if device_name.trim().is_empty()
            || device_name.len() > MAX_NAME_BYTES
            || device_name.chars().any(char::is_control)
        {
            return Err(Refusal::InvalidName);
        }
        Ok(Self {
            id,
            user_code,
            device_name,
            claim_digest,
            created_at: now,
            expires_at: now.saturating_add(ENROLLMENT_LIFETIME_MS),
            next_poll_at: now,
            state: EnrollmentState::Pending,
        })
    }

    /// # Errors
    /// Approval cannot revive cancelled, expired or completed requests.
    pub fn approve(&mut self, credential: [u8; 16], now: u64) -> Result<(), Refusal> {
        self.live(now)?;
        match self.state {
            EnrollmentState::Pending => self.state = EnrollmentState::Approved { credential },
            EnrollmentState::Approved { .. } | EnrollmentState::Claimed { .. } => {}
            EnrollmentState::Installed { .. } => return Err(Refusal::AlreadyCompleted),
            EnrollmentState::Cancelled => return Err(Refusal::Cancelled),
        }
        Ok(())
    }

    /// # Errors
    /// Only the client holding the claim secret can poll, claim or recover its result.
    pub fn poll(&mut self, claimant: Digest, now: u64) -> Result<Poll, Refusal> {
        self.authenticate(claimant)?;
        self.live(now)?;
        if now < self.next_poll_at {
            return Err(Refusal::SlowDown {
                retry_after_ms: self.next_poll_at - now,
            });
        }
        self.next_poll_at = now.saturating_add(POLL_INTERVAL_MS);
        match self.state {
            EnrollmentState::Pending => Ok(Poll::Pending),
            EnrollmentState::Approved { credential } | EnrollmentState::Claimed { credential } => {
                self.state = EnrollmentState::Claimed { credential };
                Ok(Poll::Approved { credential })
            }
            EnrollmentState::Installed { credential } => Ok(Poll::Installed { credential }),
            EnrollmentState::Cancelled => Err(Refusal::Cancelled),
        }
    }

    /// # Errors
    /// Installation acknowledges only a claimed result; repeated acknowledgements are safe.
    pub fn installed(&mut self, claimant: Digest, now: u64) -> Result<(), Refusal> {
        self.authenticate(claimant)?;
        if matches!(self.state, EnrollmentState::Installed { .. }) {
            return Ok(());
        }
        self.live(now)?;
        match self.state {
            EnrollmentState::Claimed { credential } | EnrollmentState::Installed { credential } => {
                self.state = EnrollmentState::Installed { credential }
            }
            _ => return Err(Refusal::Pending),
        }
        Ok(())
    }

    /// Returns this claimant's credential that storage must revoke atomically.
    ///
    /// # Errors
    /// A different client cannot cancel this transaction.
    pub fn cancel(&mut self, claimant: Digest) -> Result<Option<[u8; 16]>, Refusal> {
        self.authenticate(claimant)?;
        // The receipt confirms durable delivery of the key, not a successful
        // local configuration commit. Its claimant must still be able to abandon
        // an installation whose last disk write failed.
        let credential = match self.state {
            EnrollmentState::Installed { credential } => Some(credential),
            _ => self.incomplete_credential(),
        };
        self.state = EnrollmentState::Cancelled;
        Ok(credential)
    }

    #[must_use]
    pub const fn incomplete_credential(&self) -> Option<[u8; 16]> {
        match self.state {
            EnrollmentState::Approved { credential } | EnrollmentState::Claimed { credential } => {
                Some(credential)
            }
            _ => None,
        }
    }

    fn authenticate(&self, claimant: Digest) -> Result<(), Refusal> {
        if self.claim_digest == claimant {
            Ok(())
        } else {
            Err(Refusal::AccessDenied)
        }
    }

    const fn live(&self, now: u64) -> Result<(), Refusal> {
        if now >= self.expires_at {
            Err(Refusal::Expired)
        } else {
            Ok(())
        }
    }
}

#[cfg(test)]
#[path = "enrollment_tests.rs"]
mod tests;
