//! Deployment-owned TURN policy; HTTP, credentials and durable storage are host concerns.
use serde::{Deserialize, Serialize};

pub const LEASE_LIFETIME_MS: u64 = 60 * 60 * 1000;
pub const ISSUANCE_DEADLINE_MS: u64 = 30_000;

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct Subject {
    pub device: String,
    pub session: String,
    pub path: String,
}
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct Grant {
    pub urls: Vec<String>,
    pub username: String,
    pub password: String,
}
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(tag = "state", rename_all = "snake_case")]
pub enum LeaseState {
    Issuing { deadline: u64 },
    Withdrawing { deadline: u64 },
    Active { grant: Grant },
    RevocationPending { grant: Grant, next_attempt: u64 },
    Revoked,
    IssuanceFailed,
    Expired,
}
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct Lease {
    pub id: String,
    pub service_id: String,
    pub key_id: String,
    pub subject: Subject,
    pub usable_until: u64,
    pub expires_at: u64,
    pub state: LeaseState,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Revocation {
    pub lease_id: String,
    pub key_id: String,
    pub username: String,
}
impl Lease {
    pub const fn new(
        id: String,
        service_id: String,
        key_id: String,
        subject: Subject,
        now: u64,
    ) -> Self {
        Self {
            id,
            service_id,
            key_id,
            subject,
            usable_until: now.saturating_add(LEASE_LIFETIME_MS),
            expires_at: now.saturating_add(LEASE_LIFETIME_MS),
            state: LeaseState::Issuing {
                deadline: now.saturating_add(ISSUANCE_DEADLINE_MS),
            },
        }
    }
    pub fn owned_by(&self, subject: &Subject) -> bool {
        self.subject == *subject
    }
    pub fn renewable_by(&self, device: &str, session: &str, now: u64) -> bool {
        self.subject.device == device
            && self.subject.session == session
            && self.usable_until > now
            && matches!(self.state, LeaseState::Active { .. })
    }
    // Authority may change while the provider request is in flight. Retain the
    // returned grant for cleanup even when it must never be sent to the caller.
    pub fn issued(&mut self, grant: Grant, authorized: bool, now: u64) -> bool {
        // The provider starts TTL during the HTTP request. Advertise the earlier
        // bound to clients, but keep cleanup responsibility through the later bound.
        self.expires_at = now.saturating_add(LEASE_LIFETIME_MS);
        let publish = authorized
            && self.usable_until > now
            && matches!(self.state, LeaseState::Issuing { deadline } if deadline > now);
        self.state = if publish {
            LeaseState::Active { grant }
        } else {
            LeaseState::RevocationPending {
                grant,
                next_attempt: now,
            }
        };
        publish
    }
    pub fn withdraw(&mut self, now: u64) {
        self.settle_expiry(now);
        if let LeaseState::Active { grant } = &self.state {
            self.state = LeaseState::RevocationPending {
                grant: grant.clone(),
                next_attempt: now,
            };
        } else if let LeaseState::Issuing { deadline } = self.state {
            // Provider completion remains outstanding; do not report confirmed revocation.
            self.state = LeaseState::Withdrawing { deadline };
        }
    }
    pub fn issuance_failed(&mut self, now: u64) {
        self.settle_expiry(now);
        if matches!(
            self.state,
            LeaseState::Issuing { .. } | LeaseState::Withdrawing { .. }
        ) {
            self.state = LeaseState::IssuanceFailed;
        }
    }
    pub fn begin_revocation(&mut self, now: u64, retry_ms: u64) -> Option<Revocation> {
        self.settle_expiry(now);
        let LeaseState::RevocationPending {
            grant,
            next_attempt,
        } = &mut self.state
        else {
            return None;
        };
        if *next_attempt > now {
            return None;
        }
        *next_attempt = now.saturating_add(retry_ms);
        Some(Revocation {
            lease_id: self.id.clone(),
            key_id: self.key_id.clone(),
            username: grant.username.clone(),
        })
    }
    pub fn confirm_revocation(&mut self, revocation: &Revocation) -> bool {
        if self.id != revocation.lease_id {
            return false;
        }
        if self.key_id != revocation.key_id
            || !matches!(&self.state, LeaseState::RevocationPending { grant, .. } if grant.username == revocation.username)
        {
            return false;
        }
        self.state = LeaseState::Revoked;
        true
    }
    pub fn settle_expiry(&mut self, now: u64) {
        if self.expires_at <= now && !matches!(self.state, LeaseState::Revoked) {
            self.state = LeaseState::Expired;
        } else if matches!(self.state, LeaseState::Issuing { deadline } | LeaseState::Withdrawing { deadline } if deadline <= now)
        {
            self.state = LeaseState::IssuanceFailed;
        }
    }
    pub const fn pending(&self) -> bool {
        matches!(
            self.state,
            LeaseState::RevocationPending { .. } | LeaseState::Withdrawing { .. }
        )
    }
    pub const fn status(&self) -> &'static str {
        match self.state {
            LeaseState::Issuing { .. } => "issuing",
            LeaseState::Active { .. } => "active",
            LeaseState::RevocationPending { .. } | LeaseState::Withdrawing { .. } => {
                "revocation_pending"
            }
            LeaseState::Revoked => "revoked",
            LeaseState::IssuanceFailed => "issuance_failed",
            LeaseState::Expired => "expired",
        }
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(tag = "phase", rename_all = "snake_case")]
pub enum Provisioning {
    Intent { attempt: String },
    Created { attempt: String, key_id: String },
    Published { attempt: String, key_id: String },
    Abandoning { attempt: String, key_id: String },
}
impl Provisioning {
    pub fn attempt(&self) -> &str {
        match self {
            Self::Intent { attempt }
            | Self::Created { attempt, .. }
            | Self::Published { attempt, .. }
            | Self::Abandoning { attempt, .. } => attempt,
        }
    }
    pub fn key_id(&self) -> Option<&str> {
        match self {
            Self::Intent { .. } => None,
            Self::Created { key_id, .. }
            | Self::Published { key_id, .. }
            | Self::Abandoning { key_id, .. } => Some(key_id),
        }
    }
    pub const fn published(&self) -> bool {
        matches!(self, Self::Published { .. })
    }
    pub fn created(&mut self, expected_attempt: &str, key: &str) -> bool {
        if self.attempt() != expected_attempt
            || !matches!(self, Self::Intent { .. } | Self::Created { .. })
            || self.key_id().is_some_and(|id| id != key)
        {
            return false;
        }
        *self = Self::Created {
            attempt: expected_attempt.to_string(),
            key_id: key.to_string(),
        };
        true
    }
    pub fn publish(&mut self, expected_attempt: &str, secret_key_id: &str) -> bool {
        if self.attempt() != expected_attempt
            || self.key_id() != Some(secret_key_id)
            || !matches!(self, Self::Created { .. } | Self::Published { .. })
        {
            return false;
        }
        *self = Self::Published {
            attempt: expected_attempt.to_string(),
            key_id: secret_key_id.to_string(),
        };
        true
    }
}
#[cfg(test)]
#[path = "policy_tests.rs"]
mod tests;
