use super::{Actor, Environment, Event, Persistence};
use anyhow::Result;
use windshare_relay_core::{
    access::{Attribution, RegistrationDecision},
    ingress::Control,
    lifecycle::ConnectionRef,
    sessions::OpaqueHeader,
    share::{Input, Output, RelayStep},
};

impl<S: Persistence, E: Environment> Actor<S, E> {
    pub(super) fn event(&mut self, event: Event) -> Result<()> {
        let now = self.environment.now();
        let step = match event {
            Event::Connected(connection, writer) => {
                self.ports.insert(connection, writer);
                self.relay.handle(Input::Connected(connection), now)
            }
            Event::Control(connection, frame) => {
                if !self.ports.contains_key(&connection) {
                    return Ok(());
                }
                let input = match Control::inspect(connection, &frame) {
                    Control::Authorize(request) => Input::Register(
                        request.resolve(RegistrationDecision::Granted(Attribution::Anonymous), now),
                    ),
                    Control::Ready(input) => input,
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
            let queued = self
                .ports
                .get_mut(&destination)
                .is_some_and(|port| port.queue(receipt, bytes).is_ok());
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

    fn pump(&mut self, connection: ConnectionRef) -> Result<()> {
        while let Some(step) = self
            .ports
            .get_mut(&connection)
            .and_then(|port| port.submit_next(&mut self.relay, self.environment.now()))
        {
            self.outputs(step)?;
        }
        Ok(())
    }
}
