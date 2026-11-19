use super::{Actor, Environment, Persistence};
use anyhow::{Result, anyhow};
use windshare_relay_core::{
    access::{Attribution, RegistrationDecision, RegistrationRequest},
    lifecycle::Record,
    share::Input,
};

impl<S: Persistence, E: Environment> Actor<S, E> {
    pub(super) fn attribution(&self) -> Option<Attribution> {
        self.residency
            .as_ref()
            .map(crate::node::access::Residency::attribution)
    }

    fn stored_attribution(&self) -> Option<Attribution> {
        match self.authority.as_ref()?.record() {
            Record::Published(route) => Some(route.route().attribution()),
            Record::Grace(route) => Some(route.route().attribution()),
            Record::Stopping(_) | Record::Stopped(_) => None,
        }
    }

    pub(super) fn restore_residency(&mut self) -> Result<()> {
        if let Some(attribution) = self.stored_attribution() {
            self.residency = Some(
                self.node
                    .access
                    .restore(self.identity.share, attribution)
                    .map_err(|reason| anyhow!("restore resident share: {reason:?}"))?,
            );
        }
        Ok(())
    }

    pub(super) fn authorize(&mut self, request: &RegistrationRequest) -> RegistrationDecision {
        let connection = request.connection();
        let Some(port) = self.ports.get(&connection) else {
            return RegistrationDecision::Denied;
        };
        let mut decision = self.node.access.authorize(
            self.identity.share,
            port.bearer(),
            self.environment.now().0 / 1000,
        );
        if let RegistrationDecision::Granted(attribution) = decision {
            if let Some(residency) = &self.residency {
                if residency.attribution() != attribution {
                    decision = RegistrationDecision::Denied;
                }
            } else {
                match self.node.access.reserve(self.identity.share, attribution) {
                    Ok(residency) => self.residency = Some(residency),
                    Err(refusal) => decision = refusal,
                }
            }
        }
        tracing::info!(event = "registration_authorized", share = ?self.identity.share,
            attempt = ?connection, source = ?port.source(), ?decision);
        decision
    }

    pub(super) fn enforce_access(&mut self) -> Result<()> {
        // Fresh registration resolves the bearer before live policy can revoke a share.
        let Some(attribution) = self.attribution() else {
            return Ok(());
        };
        if self.node.access.allowed(self.identity.share, attribution) {
            return Ok(());
        }
        let connections: Vec<_> = self.ports.keys().copied().collect();
        if !connections.is_empty() {
            tracing::warn!(event = "share_access_revoked", share = ?self.identity.share, ?attribution);
        }
        for connection in connections {
            self.ports.remove(&connection);
            let step = self
                .relay
                .handle(Input::Disconnected(connection), self.environment.now());
            self.outputs(step)?;
        }
        Ok(())
    }
}
