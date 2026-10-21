use super::generations::Connection;
use super::{ConnectionRef, Control, Error, Observer, RelayDecision, RelayStep, ShareRelay};
use crate::{
    clock::UnixMillis,
    flow::{DeliveryConsumed, SettlementAck},
    heartbeat::ConnectionProbe,
    ingress::FrameKind,
    sessions::SessionAdmitted,
};

impl<O: Observer> ShareRelay<O> {
    pub(super) fn connection_control(
        &mut self,
        connection: ConnectionRef,
        frame: &[u8],
        now: UnixMillis,
        step: &mut RelayStep,
    ) -> Result<(), Error> {
        // Authority comes from this socket generation, never from payload fields.
        match FrameKind::classify(frame) {
            FrameKind::SessionAdmitted => {
                let admitted = SessionAdmitted::decode(frame).map_err(Error::Frame)?;
                self.admit_session(connection, admitted, now, step)
            }
            FrameKind::ConnectionProbe => {
                let result = ConnectionProbe::decode(frame)
                    .map_err(Error::Frame)
                    .and_then(|probe| self.probe(connection, probe, now, step));
                if let Err(error) = result {
                    self.trace(Some(connection), RelayDecision::Rejected(error), now);
                    self.disconnect(connection, now, step);
                }
                result
            }
            FrameKind::DeliveryConsumed => {
                let feedback = DeliveryConsumed::decode(frame).map_err(Error::Frame)?;
                self.feedback(
                    connection,
                    feedback.session(),
                    feedback.total(),
                    false,
                    now,
                    step,
                )
            }
            FrameKind::SettlementAck => {
                let ack = SettlementAck::decode(frame).map_err(Error::Frame)?;
                self.feedback(connection, ack.session(), ack.total(), true, now, step)
            }
            _ => {
                self.disconnect(connection, now, step);
                Err(Error::Unavailable)
            }
        }
    }

    fn probe(
        &mut self,
        connection: ConnectionRef,
        probe: ConnectionProbe,
        now: UnixMillis,
        step: &mut RelayStep,
    ) -> Result<(), Error> {
        let last_nonce = self
            .generations
            .connection(&connection)
            .and_then(Connection::liveness)
            .ok_or(Error::Unavailable)?
            .last_probe_nonce();
        if let Err(error) = self.generations.accept_probe(connection, probe, now) {
            if error == Error::ProbeNonce {
                self.trace(
                    Some(connection),
                    RelayDecision::ProbeRejected {
                        nonce: probe.nonce(),
                        last_nonce,
                    },
                    now,
                );
            }
            return Err(error);
        }
        Self::send(step, connection, Control::ProbeAck(probe.acknowledgement()));
        self.trace(
            Some(connection),
            RelayDecision::ProbeAccepted {
                nonce: probe.nonce(),
            },
            now,
        );
        Ok(())
    }

    pub(super) fn accepted_inbound(&mut self, connection: ConnectionRef, now: UnixMillis) {
        self.generations.accepted_inbound(connection, now);
    }

    pub(super) fn expire_liveness(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
        step: &mut RelayStep,
    ) {
        if let Some(liveness) = self
            .generations
            .connection(&connection)
            .and_then(Connection::liveness)
        {
            self.trace(
                Some(connection),
                RelayDecision::ConnectionSilent {
                    last_probe_nonce: liveness.last_probe_nonce(),
                    last_inbound: liveness.last_inbound(),
                    deadline: liveness.deadline(),
                },
                now,
            );
            // Silence revokes routing; it is not evidence that host buffers were freed.
            self.disconnect(connection, now, step);
        }
    }
}
