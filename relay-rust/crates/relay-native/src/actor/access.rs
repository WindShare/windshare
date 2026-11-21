use super::{Actor, Environment, Persistence};
use anyhow::{Result, anyhow};
use windshare_relay_core::{
    access::{Attribution, RegistrationDecision, RegistrationRequest},
    clock::UnixMillis,
    error_frame::ErrorFrame,
    ingress::FrameKind,
    lifecycle::ConnectionRef,
    share::Input,
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum RevocationReason {
    AccessPolicy,
    PublicDailyBudgetExhausted,
}

impl RevocationReason {
    const fn as_str(self) -> &'static str {
        match self {
            Self::AccessPolicy => "access_policy",
            Self::PublicDailyBudgetExhausted => "public_daily_budget_exhausted",
        }
    }

    const fn error_frame(self) -> ErrorFrame {
        match self {
            Self::AccessPolicy => ErrorFrame::AccessDenied,
            Self::PublicDailyBudgetExhausted => {
                ErrorFrame::AccessUnavailable(crate::node::SERVICE_RETRY_AFTER)
            }
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct ResidentRevocation {
    attribution: Attribution,
    reason: RevocationReason,
}

impl<S: Persistence, E: Environment> Actor<S, E> {
    pub(super) fn attribution(&self) -> Option<Attribution> {
        self.residency
            .as_ref()
            .map(crate::node::access::Residency::attribution)
    }

    pub(super) fn restore_residency(&mut self) -> Result<()> {
        if let Some(attribution) = self
            .authority
            .as_ref()
            .and_then(|stored| stored.record().live_attribution())
        {
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
                decision = RegistrationDecision::Unavailable(crate::node::SERVICE_RETRY_AFTER);
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

    pub(super) fn resident_revocation(&self) -> Option<ResidentRevocation> {
        // Fresh registration resolves the bearer before live policy can revoke a share.
        let attribution = self.attribution()?;
        if !self.node.access.allowed(self.identity.share, attribution) {
            Some(ResidentRevocation {
                attribution,
                reason: RevocationReason::AccessPolicy,
            })
        } else if self.node.public_budget_refuses_attribution(attribution) {
            Some(ResidentRevocation {
                attribution,
                reason: RevocationReason::PublicDailyBudgetExhausted,
            })
        } else {
            None
        }
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
        let Some(revocation) = self.resident_revocation() else {
            return Ok(());
        };
        self.disconnect_revoked_ports(revocation)
    }

    pub(super) fn refuse_control_access(
        &mut self,
        connection: ConnectionRef,
        kind: FrameKind,
        revocation: ResidentRevocation,
        now: UnixMillis,
    ) -> Result<()> {
        let error = revocation.reason.error_frame();
        tracing::warn!(
            event = "share_control_access_refused",
            share = ?self.identity.share,
            ?connection,
            ?kind,
            attribution = ?revocation.attribution,
            reason = revocation.reason.as_str(),
            ?error
        );
        if let Some(port) = self.ports.remove(&connection)
            && port.control(error.encode().to_vec()).is_ok()
        {
            port.finish();
        }
        let step = self.relay.handle(Input::Disconnected(connection), now);
        self.outputs(step)?;
        // Defense in depth: ensure no active sibling connection survives once
        // control admission observes a revoked share.
        self.disconnect_revoked_ports(revocation)
    }

    fn disconnect_revoked_ports(&mut self, revocation: ResidentRevocation) -> Result<()> {
        let connections: Vec<_> = self.ports.keys().copied().collect();
        if !connections.is_empty() {
            tracing::warn!(
                event = "share_access_revoked",
                share = ?self.identity.share,
                attribution = ?revocation.attribution,
                reason = revocation.reason.as_str()
            );
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
