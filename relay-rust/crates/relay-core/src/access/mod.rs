//! A host's access decision is a move-only capability for one registration.
use crate::{
    clock::UnixMillis,
    lifecycle::ConnectionRef,
    registration::{RegisterInit, RegistrationMode},
};
pub use windshare_relay_access::Attribution;

#[derive(Debug)]
pub struct RegistrationGrant {
    connection: ConnectionRef,
    init: RegisterInit,
    attribution: Attribution,
    expires_at: UnixMillis,
}

impl RegistrationGrant {
    /// Hosts resolve current service policy before minting this operation-bound grant.
    #[must_use]
    pub const fn new(
        connection: ConnectionRef,
        init: RegisterInit,
        attribution: Attribution,
        expires_at: UnixMillis,
    ) -> Self {
        Self {
            connection,
            init,
            attribution,
            expires_at,
        }
    }

    #[must_use]
    pub const fn connection(&self) -> ConnectionRef {
        self.connection
    }

    pub(crate) fn consume(
        self,
        now: UnixMillis,
    ) -> Option<(ConnectionRef, RegisterInit, Attribution)> {
        (self.init.mode() == RegistrationMode::Fresh && now < self.expires_at).then_some((
            self.connection,
            self.init,
            self.attribution,
        ))
    }
}
