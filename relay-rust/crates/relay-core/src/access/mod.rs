//! A host's access decision is bound to one validated fresh registration.
use crate::{
    clock::UnixMillis,
    error_frame::{ErrorFrame, RetryAfter},
    lifecycle::ConnectionRef,
    registration::{RegisterInit, Registration, RegistrationMode},
};
pub use windshare_relay_access::Attribution;

// This bounds the age of a resolved access decision, independently of the
// handshake deadline and the lifetime of the published share.
const REGISTRATION_GRANT_LIFETIME_MS: u64 = 30_000;
const ACCESS_RETRY_MS: u32 = 1_000;

#[derive(Clone, Copy, Debug)]
pub enum RegistrationDecision {
    Granted(Attribution),
    Denied,
    Unavailable,
}

/// Only a validated fresh registration asks the host to resolve service policy.
#[derive(Debug)]
pub struct RegistrationRequest {
    connection: ConnectionRef,
    init: RegisterInit,
}

impl RegistrationRequest {
    #[must_use]
    pub fn from_frame(connection: ConnectionRef, frame: &[u8]) -> Option<Self> {
        let init = RegisterInit::decode(frame).ok()?;
        (init.mode() == RegistrationMode::Fresh).then_some(Self { connection, init })
    }

    #[must_use]
    pub const fn connection(&self) -> ConnectionRef {
        self.connection
    }

    #[must_use]
    pub const fn registration(&self) -> &Registration {
        self.init.registration()
    }

    /// Resolve after policy I/O and revocation checks, using the decision's time.
    #[must_use]
    pub const fn resolve(
        self,
        decision: RegistrationDecision,
        now: UnixMillis,
    ) -> RegistrationAuthorization {
        match decision {
            RegistrationDecision::Granted(attribution) => {
                RegistrationAuthorization::Granted(RegistrationGrant {
                    request: self,
                    attribution,
                    expires_at: UnixMillis(now.0.saturating_add(REGISTRATION_GRANT_LIFETIME_MS)),
                })
            }
            RegistrationDecision::Denied => RegistrationAuthorization::Denied(self.connection),
            RegistrationDecision::Unavailable => {
                RegistrationAuthorization::Unavailable(self.connection)
            }
        }
    }
}

/// A resolved request is consumed exactly once by the protocol engine.
#[derive(Debug)]
pub enum RegistrationAuthorization {
    Granted(RegistrationGrant),
    Denied(ConnectionRef),
    Unavailable(ConnectionRef),
}

#[derive(Debug)]
pub struct RegistrationGrant {
    request: RegistrationRequest,
    attribution: Attribution,
    expires_at: UnixMillis,
}

impl RegistrationAuthorization {
    #[must_use]
    pub const fn connection(&self) -> ConnectionRef {
        match self {
            Self::Granted(grant) => grant.request.connection,
            Self::Denied(connection) | Self::Unavailable(connection) => *connection,
        }
    }

    pub(crate) fn consume(
        self,
        now: UnixMillis,
    ) -> Result<(RegisterInit, Attribution), ErrorFrame> {
        match self {
            Self::Granted(grant) if now < grant.expires_at => {
                Ok((grant.request.init, grant.attribution))
            }
            Self::Granted(_) | Self::Denied(_) => Err(ErrorFrame::AccessDenied),
            Self::Unavailable(_) => Err(ErrorFrame::AccessUnavailable(
                RetryAfter::from_millis(ACCESS_RETRY_MS).expect("bounded access retry"),
            )),
        }
    }
}

#[cfg(test)]
#[path = "access_tests.rs"]
mod tests;
