use super::{Actor, Environment, Event, Persistence};
use crate::node::maintenance::GateError;
use anyhow::{Context, Result};
use windshare_relay_core::{
    clock::UnixMillis,
    error_frame::ErrorFrame,
    ingress::{Control, FrameKind},
    lifecycle::ConnectionRef,
    sessions::OpaqueHeader,
    share::{Input, Output, RelayStep},
};

impl<S: Persistence, E: Environment> Actor<S, E> {
    pub(super) fn event(&mut self, event: Event) -> Result<()> {
        self.enforce_access_if_changed()?;
        let now = self.environment.now();
        let step = match event {
            Event::Connected(connection, mut writer) => {
                writer.set_wake(std::sync::Arc::clone(&self.scheduling));
                self.ports.insert(connection, *writer);
                self.relay.handle(Input::Connected(connection), now)
            }
            Event::Control(connection, frame) => {
                if !self.ports.contains_key(&connection) {
                    return Ok(());
                }
                let input = match Control::inspect(connection, &frame) {
                    Control::Authorize(request) => {
                        let decision = self.authorize(&request);
                        Input::Register(request.resolve(decision, now))
                    }
                    Control::Ready(input) => {
                        let kind = FrameKind::classify(&frame);
                        if Self::requires_resident_access(kind)
                            && let Err(gate_error) = self.admit_maintenance(connection, kind)
                        {
                            return self.refuse_maintenance_control(
                                connection,
                                kind,
                                &gate_error,
                                now,
                            );
                        }
                        if Self::requires_resident_access(kind)
                            && let Some(revocation) = self.resident_revocation()
                        {
                            return self.refuse_control_access(connection, kind, revocation, now);
                        }
                        input
                    }
                };
                let mut step = self.relay.handle(input, now);
                self.disconnect_refused(connection, &mut step);
                step
            }
            Event::Frame(connection, bytes) => return self.receive(connection, bytes),
            Event::Unsupported(connection) => {
                self.relay.handle(Input::Unsupported(connection), now)
            }
            Event::Writable(connection) => {
                self.windows.writable(connection);
                self.outputs(RelayStep::default())?;
                return self.pump(connection);
            }
            Event::Disconnected(connection) => {
                self.ports.remove(&connection);
                self.relay.handle(Input::Disconnected(connection), now)
            }
            Event::Closed(connection, applied) => {
                self.ports.remove(&connection);
                // The joined supervisor proves transport release; dropping the
                // destination port also destroys its unsubmitted queue.
                tracing::debug!(event = "connection_released", share = ?self.identity.share, ?connection);
                let step = self.relay.handle(Input::Closed(connection), now);
                let _ = applied.send(());
                step
            }
        };
        self.outputs(step)
    }

    fn receive(&mut self, source: ConnectionRef, bytes: Vec<u8>) -> Result<()> {
        if !self.ports.contains_key(&source) {
            return Ok(());
        }
        let now = self.environment.now();
        let header = match OpaqueHeader::decode(&bytes) {
            Ok(header) => header,
            Err(error) => {
                tracing::debug!(event = "opaque_frame_invalid", share = ?self.identity.share, ?source, ?error);
                let step = self.relay.handle(Input::Disconnected(source), now);
                return self.outputs(step);
            }
        };
        let mut step = self.relay.handle(Input::Frame { source, header }, now);
        self.disconnect_refused(source, &mut step);
        let receipt = step
            .outputs
            .iter()
            .position(|output| matches!(output, Output::Received(_)))
            .map(|index| step.outputs.remove(index));
        let destination = if let Some(Output::Received(receipt)) = receipt {
            let destination = receipt.flow().destination();
            let attribution = self
                .attribution()
                .context("accepted relay payload has no resident attribution")?;
            let queued = self
                .ports
                .get_mut(&destination)
                .is_some_and(|port| port.queue(receipt, bytes, attribution).is_ok());
            if !queued {
                tracing::warn!(event = "payload_queue_unavailable", share = ?self.identity.share,
                    ?source, ?destination, session = ?header.session());
                // Dropping accepted bytes is not consumption. Retire the destination
                // and let exact discard/physical-release evidence settle its debt.
                step.outputs.extend(
                    self.relay
                        .handle(Input::Disconnected(destination), now)
                        .outputs,
                );
            }
            Some(destination)
        } else {
            None
        };
        // Execute expiration/discard effects before attempting the synchronous
        // handoff. The receipt never leaves its exact payload's destination queue.
        self.outputs(step)?;
        if let Some(destination) = destination {
            self.pump(destination)?;
        }
        Ok(())
    }

    fn disconnect_refused(&mut self, connection: ConnectionRef, step: &mut RelayStep) {
        if let Some(error) = step.refusal {
            tracing::debug!(event = "ingress_refused", share = ?self.identity.share, ?connection, ?error);
            step.outputs.extend(
                self.relay
                    .handle(Input::Disconnected(connection), self.environment.now())
                    .outputs,
            );
        }
    }

    fn admit_maintenance(
        &mut self,
        connection: ConnectionRef,
        kind: FrameKind,
    ) -> Result<(), GateError> {
        let Some(gate) = self.node.maintenance.get() else {
            return Ok(());
        };
        let probe = self
            .node
            .access
            .is_probe(self.identity.share, self.attribution());
        if kind == FrameKind::Join {
            let lease = gate.admit_join(probe)?;
            self.ports
                .get_mut(&connection)
                .expect("connected port")
                .join_lease = Some(lease);
            Ok(())
        } else {
            gate.check(probe)
        }
    }

    fn refuse_maintenance_control(
        &mut self,
        connection: ConnectionRef,
        kind: FrameKind,
        gate_error: &GateError,
        now: UnixMillis,
    ) -> Result<()> {
        match gate_error {
            GateError::Maintenance => {
                tracing::debug!(
                    event = "share_control_maintenance_refused",
                    share = ?self.identity.share,
                    ?connection,
                    ?kind,
                    reason = "maintenance",
                );
            }
            GateError::Io(error) => {
                tracing::warn!(
                    event = "maintenance_gate_io_error",
                    share = ?self.identity.share,
                    ?connection,
                    ?kind,
                    %error,
                );
            }
        }
        let error = ErrorFrame::AccessUnavailable(super::access::MAINTENANCE_RETRY);
        if let Some(port) = self.ports.remove(&connection)
            && port.control(error.encode().to_vec()).is_ok()
        {
            port.finish();
        }
        let step = self.relay.handle(Input::Disconnected(connection), now);
        self.outputs(step)
    }

    // Steady-state transfer controls (`DeliveryConsumed`, `ConnectionProbe`,
    // `SettlementAck`, `SessionAdmitted`) rely on the lock-free `access_changes`
    // edge check. Handshake controls that establish or restore access to a
    // resident share must re-check live policy because a prior revocation may
    // already have been consumed while the share sat in Grace or after restart.
    const fn requires_resident_access(kind: FrameKind) -> bool {
        matches!(
            kind,
            FrameKind::RegisterInit
                | FrameKind::ResumeCredential
                | FrameKind::RegisterProof
                | FrameKind::DescriptorUpload
                | FrameKind::Join
        )
    }

    pub(super) fn pump(&mut self, connection: ConnectionRef) -> Result<()> {
        self.enforce_access_if_changed()?;
        let Some(attribution) = self.attribution() else {
            return Ok(());
        };
        while let Some(step) = self
            .ports
            .get_mut(&connection)
            .and_then(|port| port.submit_next(&mut self.relay, self.environment.now(), attribution))
        {
            self.outputs(step)?;
        }
        Ok(())
    }
}
