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
            // Stop non-owner registrations before reserving residency or writing SQLite
            // when the daily public contribution budget is already spent.
            if self.node.public_budget_refuses_attribution(attribution) {
                decision = RegistrationDecision::Unavailable;
            } else if let Some(residency) = &self.residency {
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

    // Every policy or public-budget revocation bumps `access_changes`, so the
    // steady-state frame path only performs a lock-free version check.
    pub(super) fn enforce_access_if_changed(&mut self) -> Result<()> {
        if self.access_changes.has_changed().unwrap_or(true) {
            let _ = self.access_changes.borrow_and_update();
            return self.enforce_access();
        }
        Ok(())
    }

    pub(super) fn enforce_access(&mut self) -> Result<()> {
        // Fresh registration resolves the bearer before live policy can revoke a share.
        let Some(attribution) = self.attribution() else {
            return Ok(());
        };
        let revoked_by_policy = !self.node.access.allowed(self.identity.share, attribution);
        let revoked_by_budget = self.node.public_budget_refuses_attribution(attribution);
        if !revoked_by_policy && !revoked_by_budget {
            return Ok(());
        }
        let connections: Vec<_> = self.ports.keys().copied().collect();
        if !connections.is_empty() {
            let reason = if revoked_by_policy {
                "access_policy"
            } else {
                "public_daily_budget_exhausted"
            };
            tracing::warn!(event = "share_access_revoked", share = ?self.identity.share, ?attribution, reason);
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
