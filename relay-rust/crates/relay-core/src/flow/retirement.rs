use crate::{clock::UnixMillis, lifecycle::ConnectionRef, sessions::Retirement};

use super::{
    CloseRequest, ConnectionLedger, ConnectionState, Counts, Decision, FlowKey, Input, Observer,
    Refusal, Settlement, delivery::State,
};

/// Idempotent feedback results; a stale handle never creates accounting state.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Feedback {
    /// The exact flow was already removed; its slot may now belong to another flow.
    Stale,
    /// Duplicate or older cumulative consumption, without any refund.
    Unchanged,
    /// Only these newly consumed resources were released.
    Released(Counts),
    /// A retired flow no longer owns any resources and its slot was released.
    Settled(Counts),
}

impl<O: Observer> ConnectionLedger<O> {
    /// Applies session retirement to the direction targeting this connection.
    ///
    /// Revoked grants are immediately reusable. Accepted payloads remain charged
    /// until `discarded`; submitted payloads remain charged after that too.
    /// For STOP, the host waits for durable intent before physical queue cleanup.
    /// Repeating retirement keeps its original deadline.
    ///
    /// # Errors
    ///
    /// Refuses a retirement from another relay. Missing/already-settled flows
    /// return `None` without touching sibling sessions.
    pub fn retire(
        &mut self,
        retirement: Retirement,
        now: UnixMillis,
    ) -> Result<Option<FlowKey>, Refusal> {
        let input = Input::Retire(retirement);
        self.run(input, now, |ledger| {
            if retirement.session.relay() != ledger.relay {
                return Err(Refusal::WrongDestination);
            }
            let Some(mut entry) = ledger
                .entries
                .iter()
                .find(|entry| entry.key.session() == retirement.session)
                .copied()
            else {
                return Ok(None);
            };
            if entry.state == State::Open {
                entry.state = State::Discarding {
                    deadline: now.saturating_add(ledger.limits.settlement_timeout),
                };
                ledger.replace(entry.key.slot, Some(entry));
                ledger.record(input, Decision::Retiring(entry.key), now);
            }
            Ok(Some(entry.key))
        })
    }

    /// Confirms unsent payloads are physically gone and returns the ordered notice.
    ///
    /// Issue the returned notice after all submitted data on this destination.
    /// If everything submitted was already consumed, the slot can be freed here;
    /// the notice still tells the peer to retire the session. Duplicate cleanup
    /// is harmless and never renews a settlement deadline.
    ///
    /// # Errors
    ///
    /// Refuses cleanup of an open flow or a different destination. Removed flows
    /// return `None`, so delayed callbacks cannot touch a replacement slot.
    pub fn discarded(
        &mut self,
        flow: FlowKey,
        now: UnixMillis,
    ) -> Result<Option<Settlement>, Refusal> {
        let input = Input::Discarded(flow);
        self.run(input, now, |ledger| {
            ledger.check_destination(flow, flow.destination())?;
            let Ok(mut entry) = ledger.delivery(flow) else {
                return Ok(None);
            };
            let deadline = match entry.state {
                State::Open => return Err(Refusal::NotRetired),
                State::Discarding { deadline } | State::Settling { deadline } => deadline,
            };
            let notice = entry.settlement();
            if entry.submitted == entry.consumed {
                ledger.replace(flow.slot, None);
                ledger.record(input, Decision::Settled(flow), now);
            } else if matches!(entry.state, State::Discarding { .. }) {
                entry.state = State::Settling { deadline };
                ledger.replace(flow.slot, Some(entry));
                ledger.record(input, Decision::AwaitingSettlement(flow), now);
            }
            Ok(Some(notice))
        })
    }

    /// Applies cumulative application consumption from the exact destination.
    ///
    /// The adapter must authenticate the connection and map its wire feedback to
    /// this exact direction. An older prefix is harmless; a mixed regression or
    /// a count beyond submitted data is not a valid prefix.
    ///
    /// # Errors
    ///
    /// Refuses the wrong connection generation or inconsistent consumption.
    pub fn consume(
        &mut self,
        flow: FlowKey,
        from: ConnectionRef,
        total: Counts,
        now: UnixMillis,
    ) -> Result<Feedback, Refusal> {
        let input = Input::Consume { flow, from, total };
        self.run(input, now, |ledger| {
            ledger.check_destination(flow, from)?;
            let Ok(mut entry) = ledger.delivery(flow) else {
                return Ok(Feedback::Stale);
            };
            if entry.consumed.covers(total) {
                return Ok(Feedback::Unchanged);
            }
            if !total.covers(entry.consumed) || !entry.submitted.covers(total) {
                return Err(Refusal::InvalidConsumption);
            }
            let released = total.sub(entry.consumed);
            if !released.is_envelope_batch() || !entry.submitted.sub(total).is_envelope_batch() {
                return Err(Refusal::InvalidConsumption);
            }
            entry.consumed = total;
            if matches!(entry.state, State::Settling { .. }) && total == entry.submitted {
                ledger.replace(flow.slot, None);
                ledger.record(input, Decision::Settled(flow), now);
                Ok(Feedback::Settled(released))
            } else {
                ledger.replace(flow.slot, Some(entry));
                Ok(Feedback::Released(released))
            }
        })
    }

    /// Settles exactly the frozen submitted totals after the ordered notice.
    ///
    /// This frees connection capacity only; no session or routing authority can
    /// be revived. All earlier partial consumption is subtracted exactly once.
    ///
    /// # Errors
    ///
    /// Refuses wrong generations, premature settlement or inconsistent final totals.
    pub fn settle(
        &mut self,
        flow: FlowKey,
        from: ConnectionRef,
        total: Counts,
        now: UnixMillis,
    ) -> Result<Feedback, Refusal> {
        let input = Input::Settle { flow, from, total };
        self.run(input, now, |ledger| {
            ledger.check_destination(flow, from)?;
            let Ok(entry) = ledger.delivery(flow) else {
                return Ok(Feedback::Stale);
            };
            match entry.state {
                State::Open => return Err(Refusal::NotRetired),
                State::Discarding { .. } => return Err(Refusal::CleanupPending),
                State::Settling { .. } => {}
            }
            if total != entry.submitted {
                return Err(Refusal::InvalidSettlement);
            }
            let released = entry.submitted.sub(entry.consumed);
            ledger.replace(flow.slot, None);
            ledger.record(input, Decision::Settled(flow), now);
            Ok(Feedback::Settled(released))
        })
    }

    /// Earliest fixed retirement deadline to combine with other engine deadlines.
    #[must_use]
    pub fn deadline(&self) -> Option<UnixMillis> {
        if self.state != ConnectionState::Open {
            return None;
        }
        self.next_expiration().map(|request| request.deadline)
    }

    /// A pending close obligation survives feedback until actual closure is confirmed.
    #[must_use]
    pub const fn close_request(&self) -> Option<CloseRequest> {
        match self.state {
            ConnectionState::Closing(request) => Some(request),
            ConnectionState::Open | ConnectionState::Closed => None,
        }
    }

    /// Checks absolute cleanup time without treating other traffic as progress.
    #[must_use]
    pub fn tick(&mut self, now: UnixMillis) -> Option<CloseRequest> {
        self.expire(now);
        self.close_request()
    }

    /// Confirms physical termination and releases every remaining stage exactly once.
    ///
    /// A close request, a socket error, or calling a close API is insufficient:
    /// the host must know its queues and transport buffers no longer retain data.
    ///
    /// # Errors
    ///
    /// Rejects another connection generation; a repeated confirmation releases zero.
    pub fn confirm_closed(
        &mut self,
        destination: ConnectionRef,
        now: UnixMillis,
    ) -> Result<Counts, Refusal> {
        let input = Input::Closed(destination);
        self.run(input, now, |ledger| {
            if destination != ledger.destination {
                return Err(Refusal::WrongDestination);
            }
            if ledger.state == ConnectionState::Closed {
                return Ok(Counts::ZERO);
            }
            let reclaimed = ledger.usage.total();
            ledger.entries.clear();
            ledger.usage = super::Usage::default();
            ledger.startup_used = Counts::ZERO;
            ledger.state = ConnectionState::Closed;
            ledger.checkpoint_changed = true;
            ledger.next_expiration = None;
            ledger.record(input, Decision::Closed { reclaimed }, now);
            Ok(reclaimed)
        })
    }

    const fn next_expiration(&self) -> Option<CloseRequest> {
        self.next_expiration
    }

    pub(super) fn expire(&mut self, now: UnixMillis) {
        if self.state == ConnectionState::Open
            && let Some(request) = self.next_expiration()
            && now >= request.deadline
        {
            self.state = ConnectionState::Closing(request);
            self.checkpoint_changed = true;
            self.record(Input::Tick, Decision::Closing(request), now);
        }
    }
}
