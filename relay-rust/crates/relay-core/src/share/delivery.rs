use super::RuntimeStatus;
use super::{
    ConnectionRef, Control, Error, Input, Observer, Output, RelayDecision, Role, ShareRelay,
    Submission, Turn,
};
use crate::{
    clock::UnixMillis,
    flow::{Counts, DeliveryConsumed, FlowKey, ReceivedFrame, SettlementAck},
    sessions::{Action, Event as SessionEvent, OpaqueHeader},
};

impl<O: Observer> ShareRelay<O> {
    /// Current responsibility for this exact generation, including detached sockets.
    /// Hosts use this for diagnostics; it does not grant forwarding or release authority.
    #[must_use]
    pub fn delivery_usage(&self, connection: ConnectionRef) -> Option<crate::flow::Usage> {
        self.ledgers
            .get(&connection)
            .map(crate::flow::ConnectionLedger::usage)
    }

    pub(super) fn delivery_control(
        &mut self,
        connection: ConnectionRef,
        frame: &[u8],
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        // The socket supplies authority; payload fields can never choose a different
        // connection generation or turn a retirement receipt into ordinary credit.
        let input = match frame.first_chunk::<4>() {
            Some(&DeliveryConsumed::MAGIC) => {
                let feedback = DeliveryConsumed::decode(frame).map_err(Error::Frame)?;
                Input::Consumed {
                    connection,
                    session: feedback.session(),
                    total: feedback.total(),
                }
            }
            Some(&SettlementAck::MAGIC) => {
                let acknowledgement = SettlementAck::decode(frame).map_err(Error::Frame)?;
                Input::Settled {
                    connection,
                    session: acknowledgement.session(),
                    total: acknowledgement.total(),
                }
            }
            _ => {
                self.disconnect(connection, now, turn);
                return Err(Error::Unavailable);
            }
        };
        self.apply(input, now, turn)
    }

    pub(super) fn receive(
        &mut self,
        source: ConnectionRef,
        header: OpaqueHeader,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        if self.runtime == RuntimeStatus::ReloadRequired {
            return Err(Error::Unavailable);
        }
        let id = header.session();
        let action = self.session_event(id, SessionEvent::Frame { source, header }, now, turn);
        let Some(Action::Forward(permit)) = action else {
            return Err(Error::Unavailable);
        };
        let entry = self.sessions.get(&id).ok_or(Error::UnknownSession)?;
        let flow = *entry
            .flows
            .iter()
            .find(|flow| flow.permit() == permit)
            .ok_or(Error::UnknownSession)?;
        let route = entry
            .session
            .check_forward(&self.lifecycle, permit, now)
            .map_err(Error::Forward)?;
        let destination = flow.destination();
        let result = self
            .ledgers
            .get_mut(&destination)
            .ok_or(Error::UnknownConnection)?
            .receive(flow, &route, header, now)
            .map_err(Error::Flow);
        self.flush_ledger(destination, now, turn);
        turn.outputs.push(Output::Received(result?));
        Ok(())
    }

    /// Revalidate and charge at the actual synchronous host handoff. A bounded
    /// writer may enqueue already-committed bytes here; awaiting socket readiness
    /// belongs before this call. A failed/uncertain handoff never refunds occupancy.
    ///
    /// # Errors
    /// The receipt must still be current and next in FIFO order. The callback is
    /// never invoked on refusal, including a STOP since receipt creation.
    pub fn submit(
        &mut self,
        receipt: &ReceivedFrame,
        now: UnixMillis,
        handoff: impl FnOnce(FlowKey) -> Submission,
    ) -> Turn {
        let mut turn = Turn::default();
        self.expire(now, &mut turn);
        let result = self.submit_frame(receipt, now, handoff);
        if result == Ok(Submission::Uncertain) {
            self.disconnect(receipt.flow().destination(), now, &mut turn);
        }
        self.flush_ledger(receipt.flow().destination(), now, &mut turn);
        self.progress_stop(now, &mut turn);
        self.start_work(now, &mut turn);
        turn.deadline = self.deadlines.next();
        if let Err(error) = result {
            turn.refusal = Some(error);
            self.trace(
                Some(receipt.flow().destination()),
                RelayDecision::Rejected(error),
                now,
            );
        }
        turn
    }

    fn submit_frame(
        &mut self,
        receipt: &ReceivedFrame,
        now: UnixMillis,
        handoff: impl FnOnce(FlowKey) -> Submission,
    ) -> Result<Submission, Error> {
        if self.runtime == RuntimeStatus::ReloadRequired {
            return Err(Error::Unavailable);
        }
        let flow = receipt.flow();
        let entry = self
            .sessions
            .get(&flow.session().wire_id())
            .ok_or(Error::UnknownSession)?;
        let route = entry
            .session
            .check_forward(&self.lifecycle, flow.permit(), now)
            .map_err(Error::Forward)?;
        let connection = self
            .connections
            .get(&flow.destination())
            .ok_or(Error::UnknownConnection)?;
        if matches!(connection.role, Role::Closing) {
            return Err(Error::Unavailable);
        }
        self.ledgers
            .get_mut(&flow.destination())
            .ok_or(Error::UnknownConnection)?
            .submit(&route, receipt, now)
            .map_err(Error::Flow)?;
        Ok(handoff(flow))
    }

    pub(super) fn grant(
        &mut self,
        flow: FlowKey,
        credit: Counts,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        if self.runtime == RuntimeStatus::ReloadRequired {
            return Err(Error::Unavailable);
        }
        let entry = self
            .sessions
            .get(&flow.session().wire_id())
            .ok_or(Error::UnknownSession)?;
        let route = entry
            .session
            .check_forward(&self.lifecycle, flow.permit(), now)
            .map_err(Error::Forward)?;
        let result = self
            .ledgers
            .get_mut(&flow.destination())
            .ok_or(Error::UnknownConnection)?
            .grant(flow, &route, credit, now)
            .map_err(Error::Flow);
        self.flush_ledger(flow.destination(), now, turn);
        Self::send(turn, flow.permit().source(), Control::Grant(result?));
        Ok(())
    }

    pub(super) fn discarded(
        &mut self,
        flow: FlowKey,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        if self
            .sessions
            .get(&flow.session().wire_id())
            .is_some_and(|entry| entry.deferred)
        {
            return Err(Error::Unavailable);
        }
        let Some(ledger) = self.ledgers.get_mut(&flow.destination()) else {
            return Ok(());
        };
        let result = ledger.discarded(flow, now).map_err(Error::Flow);
        self.flush_ledger(flow.destination(), now, turn);
        if let Some(notice) = result?
            && self.connections.contains_key(&flow.destination())
        {
            Self::send(
                turn,
                flow.destination(),
                Control::Settlement(notice.try_into().map_err(Error::Frame)?),
            );
        }
        self.collect_session(flow.session(), now, turn);
        self.collect_ledger(flow.destination(), now);
        Ok(())
    }

    pub(super) fn feedback(
        &mut self,
        connection: ConnectionRef,
        session: crate::identifiers::RelaySessionId,
        total: Counts,
        final_receipt: bool,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        let Some(&flow) = self.routes.get(&(connection, session)) else {
            return Ok(());
        };
        let Some(ledger) = self.ledgers.get_mut(&flow.destination()) else {
            return Ok(());
        };
        let result = if final_receipt {
            ledger.settle(flow, connection, total, now)
        } else {
            ledger.consume(flow, connection, total, now)
        };
        self.flush_ledger(flow.destination(), now, turn);
        result.map_err(Error::Flow)?;
        self.collect_session(flow.session(), now, turn);
        self.collect_ledger(flow.destination(), now);
        Ok(())
    }
}
