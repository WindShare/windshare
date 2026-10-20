use super::{Actor, Environment, Event, Persistence};
use anyhow::Result;
use windshare_relay_core::{
    lifecycle::ConnectionRef,
    sessions::OpaqueHeader,
    share::{Control, Input, Output, Turn},
};

const REGISTRATION_GRANT_LIFETIME_MS: u64 = 30_000;

impl<S: Persistence, E: Environment> Actor<S, E> {
    pub(super) fn event(&mut self, event: Event) -> Result<()> {
        let now = self.environment.now();
        let turn = match event {
            Event::Connected(connection, writer) => {
                self.ports.insert(connection, writer);
                self.relay.handle(Input::Connected(connection), now)
            }
            Event::Control(connection, frame) => {
                if !self.ports.contains_key(&connection) {
                    return Ok(());
                }
                if matches!(frame.get(..4), Some(b"WS2B" | b"WS2S")) {
                    self.windows.wake(connection);
                }
                let input = match windshare_relay_core::registration::RegisterInit::decode(&frame) {
                    Ok(init)
                        if init.mode()
                            == windshare_relay_core::registration::RegistrationMode::Fresh =>
                    {
                        Input::Register(windshare_relay_core::access::RegistrationGrant::new(
                            connection,
                            init,
                            windshare_relay_core::access::Attribution::Anonymous,
                            windshare_relay_core::clock::UnixMillis(
                                now.0.saturating_add(REGISTRATION_GRANT_LIFETIME_MS),
                            ),
                        ))
                    }
                    _ => Input::Control {
                        connection,
                        frame: &frame,
                    },
                };
                let mut turn = self.relay.handle(input, now);
                self.disconnect_refused(connection, &mut turn);
                turn
            }
            Event::Frame(connection, bytes) => return self.receive(connection, bytes),
            Event::Unsupported(connection) => {
                let mut turn = Turn::default();
                turn.outputs.push(Output::Send {
                    connection,
                    control: Control::Error(
                        windshare_relay_core::error_frame::ErrorFrame::UnsupportedMode,
                    ),
                });
                turn.outputs.extend(
                    self.relay
                        .handle(Input::Disconnected(connection), now)
                        .outputs,
                );
                turn
            }
            Event::Writable(connection) => {
                self.windows.writable(connection);
                self.outputs(Turn::default())?;
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
                let turn = self.relay.handle(Input::Closed(connection), now);
                let _ = applied.send(());
                turn
            }
        };
        self.outputs(turn)
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
                let turn = self.relay.handle(Input::Disconnected(source), now);
                return self.outputs(turn);
            }
        };
        let mut turn = self.relay.handle(Input::Frame { source, header }, now);
        self.disconnect_refused(source, &mut turn);
        let receipt = turn
            .outputs
            .iter()
            .position(|output| matches!(output, Output::Received(_)))
            .map(|index| turn.outputs.remove(index));
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
                turn.outputs.extend(
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
        self.outputs(turn)?;
        if let Some(destination) = destination {
            self.pump(destination)?;
        }
        Ok(())
    }

    fn disconnect_refused(&mut self, connection: ConnectionRef, turn: &mut Turn) {
        if let Some(error) = turn.refusal {
            tracing::debug!(event = "ingress_refused", share = ?self.identity.share, ?connection, ?error);
            turn.outputs.extend(
                self.relay
                    .handle(Input::Disconnected(connection), self.environment.now())
                    .outputs,
            );
        }
    }

    fn pump(&mut self, connection: ConnectionRef) -> Result<()> {
        while let Some(turn) = self
            .ports
            .get_mut(&connection)
            .and_then(|port| port.submit_next(&mut self.relay, self.environment.now()))
        {
            self.outputs(turn)?;
        }
        Ok(())
    }
}
