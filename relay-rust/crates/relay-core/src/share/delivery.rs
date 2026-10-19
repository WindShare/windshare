use super::RuntimeStatus;
use super::{
    ConnectionRef, Control, Error, Observer, Output, RelayDecision, Role, ShareRelay, Submission,
    Turn,
};
use crate::{
    clock::UnixMillis,
    flow::{Counts, Feedback, FlowKey, ReceivedFrame},
    observation::SessionInputKind,
    sessions::{Action, Event as SessionEvent, OpaqueHeader},
};

impl<O: Observer> ShareRelay<O> {
    /// Current responsibility for this exact generation, including detached sockets.
    /// Hosts use this for diagnostics; it does not grant forwarding or release authority.
    #[must_use]
    pub fn delivery_usage(&self, connection: ConnectionRef) -> Option<crate::flow::Usage> {
        self.generations
            .ledger(&connection)
            .map(crate::flow::ConnectionLedger::usage)
    }

    /// Read-only replenishment offer for a host-selected outstanding window.
    /// Grants, queued payloads and unconsumed submissions all occupy the window;
    /// duplicate feedback therefore cannot create additional credit. Hosts must
    /// still reserve this offer through `Input::Grant`, which rechecks authority.
    #[must_use]
    pub fn delivery_window(&self, flow: FlowKey, target: Counts) -> Option<Counts> {
        self.generations
            .ledger(&flow.destination())?
            .window_credit(flow, target)
    }

    /// Enumerate current open flows without a host-maintained copy of routing state.
    /// Hibernating hosts can resume scheduling from restored ledgers; this view
    /// grants no credit and must still be rechecked through `Input::Grant`.
    pub fn delivery_flows(&self) -> impl Iterator<Item = FlowKey> + '_ {
        self.routes
            .values()
            .copied()
            .filter(|flow| self.delivery_window(*flow, Counts::ZERO).is_some())
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
        let permit = match action {
            Some(Action::Forward(permit)) => permit,
            Some(Action::Discard(_)) => return Ok(()),
            Some(Action::Refused(reason)) => return Err(Error::Session(reason)),
            None => {
                return self.discard_collected_session(source, id, SessionInputKind::Frame, now);
            }
            Some(Action::Active | Action::None) => unreachable!("frame must route or be rejected"),
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
            .generations
            .update_ledger(
                destination,
                |ledger| ledger.receive(flow, &route, header, now),
                &mut self.checkpointing,
                &mut self.deadlines,
                &mut self.observer,
            )
            .ok_or(Error::UnknownConnection)?
            .map_err(Error::Flow);
        self.apply_delivery_close(destination, now, turn);
        turn.outputs.push(Output::Received(result?));
        self.accepted_inbound(source, now);
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
        self.apply_delivery_close(receipt.flow().destination(), now, &mut turn);
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
            .generations
            .connection(&flow.destination())
            .ok_or(Error::UnknownConnection)?;
        if matches!(connection.role, Role::Closing) {
            return Err(Error::Unavailable);
        }
        self.generations
            .update_ledger(
                flow.destination(),
                |ledger| ledger.submit(&route, receipt, now),
                &mut self.checkpointing,
                &mut self.deadlines,
                &mut self.observer,
            )
            .ok_or(Error::UnknownConnection)?
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
            .generations
            .update_ledger(
                flow.destination(),
                |ledger| ledger.grant(flow, &route, credit, now),
                &mut self.checkpointing,
                &mut self.deadlines,
                &mut self.observer,
            )
            .ok_or(Error::UnknownConnection)?
            .map_err(Error::Flow);
        self.apply_delivery_close(flow.destination(), now, turn);
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
        let Some(result) = self.generations.update_ledger(
            flow.destination(),
            |ledger| ledger.discarded(flow, now),
            &mut self.checkpointing,
            &mut self.deadlines,
            &mut self.observer,
        ) else {
            return Ok(());
        };
        let result = result.map_err(Error::Flow);
        self.apply_delivery_close(flow.destination(), now, turn);
        if let Some(notice) = result?
            && self.generations.connection(&flow.destination()).is_some()
        {
            Self::send(
                turn,
                flow.destination(),
                Control::Settlement(notice.try_into().map_err(Error::Frame)?),
            );
        }
        self.collect_session(flow.session(), now, turn);
        self.collect_ledger(flow.destination(), now);
        self.progress_retirements(now);
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
        let Some(result) = self.generations.update_ledger(
            flow.destination(),
            |ledger| {
                if final_receipt {
                    ledger.settle(flow, connection, total, now)
                } else {
                    ledger.consume(flow, connection, total, now)
                }
            },
            &mut self.checkpointing,
            &mut self.deadlines,
            &mut self.observer,
        ) else {
            return Ok(());
        };
        self.apply_delivery_close(flow.destination(), now, turn);
        if result.map_err(Error::Flow)? != Feedback::Stale {
            self.accepted_inbound(connection, now);
        }
        let retirement_affected = self.generations.retirement_state(&connection).is_some()
            || self
                .generations
                .retirement_state(&flow.permit().source())
                .is_some();
        self.collect_session(flow.session(), now, turn);
        self.collect_ledger(flow.destination(), now);
        if retirement_affected {
            self.progress_retirements(now);
        }
        Ok(())
    }
}
