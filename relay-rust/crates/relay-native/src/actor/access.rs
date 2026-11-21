use super::{Actor, Environment, Persistence};
use crate::node::{
    access::{Authorization, Refusal, RefusalReason},
    maintenance::GateError,
};
use anyhow::{Result, anyhow};
use windshare_relay_core::{
    access::{Attribution, RegistrationDecision, RegistrationRequest},
    clock::UnixMillis,
    error_frame::ErrorFrame,
    ingress::FrameKind,
    lifecycle::ConnectionRef,
    share::Input,
};

pub(super) const MAINTENANCE_RETRY: windshare_relay_core::error_frame::RetryAfter =
    windshare_relay_core::error_frame::RetryAfter::from_millis(1000).expect("one second retry");

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

    fn has_terminal_authority(&self) -> bool {
        self.authority
            .as_ref()
            .is_some_and(|stored| stored.record().live_attribution().is_none())
    }

    // Permanent STOP records stay in SQLite without retaining a live residency slot;
    // a vacant share only holds provisional residency while registration is active.
    pub(super) fn sync_residency(&mut self) {
        let release = match self.authority.as_ref() {
            Some(stored) => stored.record().live_attribution().is_none(),
            None => self.ports.is_empty() && self.commit.is_none(),
        };
        if release && let Some(residency) = self.residency.take() {
            let attribution = residency.attribution();
            let stopped = self.has_terminal_authority();
            if stopped {
                // Already-admitted sockets (such as an uncredentialed STOP draining
                // WS2Y/Close) keep this share's owner-egress attribution after the
                // concurrency slot is freed.
                for port in self.ports.values() {
                    port.retain_share_attribution(attribution);
                }
            }
            tracing::debug!(
                event = "share_residency_released",
                share = ?self.identity.share,
                ?attribution,
                stopped,
            );
        }
    }

    fn maintenance_refusal(
        &self,
        connection: ConnectionRef,
        attribution: Attribution,
    ) -> Option<Refusal> {
        let gate = self.node.maintenance.get()?;
        let probe = self
            .node
            .access
            .is_probe(self.identity.share, Some(attribution));
        match gate.check(probe) {
            Ok(()) => None,
            Err(GateError::Maintenance) => Some(Refusal::unavailable(RefusalReason::NodeDraining)),
            Err(GateError::Io(error)) => {
                tracing::warn!(
                    event = "maintenance_gate_io_error",
                    share = ?self.identity.share,
                    attempt = ?connection,
                    operation = "authorize",
                    %error,
                );
                Some(Refusal::unavailable(RefusalReason::MaintenanceGateError))
            }
        }
    }

    pub(super) fn authorize(&mut self, request: &RegistrationRequest) -> RegistrationDecision {
        let connection = request.connection();
        let Some(port) = self.ports.get(&connection) else {
            tracing::info!(
                event = "registration_authorized",
                share = ?self.identity.share,
                attempt = ?connection,
                decision = ?RegistrationDecision::Denied,
                reason = RefusalReason::ConnectionMissing.as_str(),
            );
            return RegistrationDecision::Denied;
        };
        let outcome = match self.node.access.authorize(
            self.identity.share,
            port.bearer(),
            self.environment.now().0 / 1000,
        ) {
            Authorization::Granted {
                attribution,
                class,
                allowance,
            } => {
                // Stop non-owner registrations before reserving residency or writing SQLite
                // when the local maintenance fence or daily public budget refuses admission.
                if let Some(refusal) = self.maintenance_refusal(connection, attribution) {
                    Authorization::Refused(refusal)
                } else if self.node.public_budget_refuses_attribution(attribution) {
                    Authorization::Refused(Refusal {
                        decision: RegistrationDecision::Unavailable(
                            crate::node::SERVICE_RETRY_AFTER,
                        ),
                        reason: RefusalReason::PublicDailyBudgetExhausted,
                    })
                } else if let Some(residency) = &self.residency {
                    if residency.attribution() == attribution {
                        Authorization::Granted {
                            attribution,
                            class,
                            allowance,
                        }
                    } else {
                        Authorization::Refused(Refusal::denied(RefusalReason::AttributionMismatch))
                    }
                } else if self.has_terminal_authority() {
                    // Stopping/Stopped records cannot become live again; let the kernel
                    // return StopPending/Stopped/Occupied without reserving a slot.
                    Authorization::Granted {
                        attribution,
                        class,
                        allowance,
                    }
                } else {
                    match self.node.access.reserve(self.identity.share, attribution) {
                        Ok(residency) => {
                            self.residency = Some(residency);
                            Authorization::Granted {
                                attribution,
                                class,
                                allowance,
                            }
                        }
                        Err(refusal) => Authorization::Refused(refusal),
                    }
                }
            }
            refused @ Authorization::Refused(_) => refused,
        };
        match outcome {
            Authorization::Granted {
                attribution,
                class,
                allowance,
            } => {
                let decision = RegistrationDecision::Granted(attribution);
                tracing::info!(
                    event = "registration_authorized",
                    share = ?self.identity.share,
                    attempt = ?connection,
                    source = ?port.source(),
                    ?decision,
                    ?class,
                    ?allowance,
                );
                decision
            }
            Authorization::Refused(refusal) => {
                tracing::info!(
                    event = "registration_authorized",
                    share = ?self.identity.share,
                    attempt = ?connection,
                    source = ?port.source(),
                    decision = ?refusal.decision,
                    reason = refusal.reason.as_str(),
                );
                refusal.decision
            }
        }
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
