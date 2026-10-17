use crate::{
    clock::UnixMillis,
    lifecycle::Observer as LifecycleObserver,
    sessions::{OpaqueHeader, ValidatedForward},
};

use super::{
    ConnectionLedger, Counts, Decision, FlowKey, Grant, Input, Observer, ReceivedFrame, Refusal,
    delivery::Delivery,
};

impl<const N: usize, O: Observer> ConnectionLedger<N, O> {
    /// Reserves startup credit before the first frame, using a live session route.
    ///
    /// Hosts open both directions before advertising initial credit. If opening
    /// the other destination fails, retire the session and clean up the first.
    ///
    /// # Errors
    ///
    /// Refuses a wrong destination, duplicate route, exhausted startup/metadata
    /// capacity, or a connection whose cleanup deadline has expired.
    pub fn open<L: LifecycleObserver>(
        &mut self,
        route: &ValidatedForward<'_, L>,
        now: UnixMillis,
    ) -> Result<Grant, Refusal> {
        let permit = route.permit();
        let input = Input::Open(permit);
        self.run(input, now, |ledger| {
            ledger.ensure_open()?;
            if permit.session().relay() != ledger.relay
                || permit.destination() != ledger.destination
            {
                return Err(Refusal::WrongDestination);
            }
            if ledger
                .entries
                .iter()
                .flatten()
                .any(|entry| entry.key.permit == permit)
            {
                return Err(Refusal::AlreadyOpen);
            }
            let credit = ledger.limits.startup_grant;
            if !ledger.receive_available()?.covers(credit) {
                return Err(Refusal::StartupCapacity);
            }
            let startup = credit.min(ledger.limits.startup_reserve.sub(ledger.startup_used));
            // A prior handshake may retain unused byte credit after consuming its
            // frame allowance. Free ordinary capacity must still admit newcomers.
            if !ledger
                .limits
                .regular()
                .sub(ledger.regular_used())
                .covers(credit.sub(startup))
            {
                return Err(Refusal::StartupCapacity);
            }
            let slot = ledger
                .entries
                .iter()
                .position(Option::is_none)
                .ok_or(Refusal::FlowCapacity)?;
            let flow = FlowKey { slot, permit };
            ledger.replace(slot, Some(Delivery::new(flow, credit, startup)));
            ledger.record(input, Decision::Opened(flow), now);
            Ok(Grant { flow, credit })
        })
    }

    /// Reserves ordinary capacity before issuing additional source credit.
    ///
    /// Grant failure changes no counters. Delivery failure after a successful
    /// grant requires retirement; it cannot be treated as a consumption refund.
    ///
    /// # Errors
    ///
    /// Refuses stale authority, retirement, empty grants, counter overflow or
    /// exhaustion of either ordinary resource dimension.
    pub fn grant<L: LifecycleObserver>(
        &mut self,
        flow: FlowKey,
        route: &ValidatedForward<'_, L>,
        credit: Counts,
        now: UnixMillis,
    ) -> Result<Grant, Refusal> {
        let input = Input::Grant { flow, credit };
        self.run(input, now, |ledger| {
            let mut entry = ledger.live_delivery(flow, route)?;
            if credit == Counts::ZERO {
                return Err(Refusal::EmptyGrant);
            }
            let granted = entry
                .granted
                .checked_add(credit)
                .ok_or(Refusal::CounterExhausted)?;
            if !ledger.receive_available()?.covers(credit) {
                return Err(Refusal::DeliveryCapacity);
            }
            if !ledger
                .limits
                .regular()
                .sub(ledger.regular_used())
                .covers(credit)
            {
                return Err(Refusal::DeliveryCapacity);
            }
            entry.granted = granted;
            ledger.replace(flow.slot, Some(entry));
            Ok(Grant { flow, credit })
        })
    }

    /// Charges a complete validated envelope against already-reserved credit.
    ///
    /// Retain the returned receipt with the host-owned payload. Receiving and
    /// queueing do not free either resource dimension.
    ///
    /// # Errors
    ///
    /// Refuses stale/retired authority, the wrong envelope session or insufficient
    /// frame/encoded-byte credit. No partial debit is possible.
    pub fn receive<L: LifecycleObserver>(
        &mut self,
        flow: FlowKey,
        route: &ValidatedForward<'_, L>,
        header: OpaqueHeader,
        now: UnixMillis,
    ) -> Result<ReceivedFrame, Refusal> {
        let input = Input::Receive { flow, header };
        self.run(input, now, |ledger| {
            let mut entry = ledger.live_delivery(flow, route)?;
            if header.session() != flow.session().wire_id() {
                return Err(Refusal::WrongSession);
            }
            let cost = Counts::new(1, header.encoded_bytes() as u64);
            if !entry.granted.sub(entry.received).covers(cost) {
                return Err(Refusal::InsufficientCredit);
            }
            entry.received = entry.received.add(cost);
            ledger.replace(flow.slot, Some(entry));
            Ok(ReceivedFrame {
                flow,
                ordinal: entry.received.frames,
                encoded_bytes: cost.encoded_bytes,
            })
        })
    }

    /// Charges submission immediately before synchronously handing bytes to the host.
    ///
    /// Keep `route` borrowed through the actual host call. An await/queued writer
    /// must revalidate first. A write error can be uncertain, so it retires the
    /// channel rather than rolling this charge back. There is intentionally no
    /// socket-write-completion refund API.
    ///
    /// # Errors
    ///
    /// Refuses stale/retired authority or a duplicate/out-of-order receipt.
    pub fn submit<L: LifecycleObserver>(
        &mut self,
        route: &ValidatedForward<'_, L>,
        frame: &ReceivedFrame,
        now: UnixMillis,
    ) -> Result<(), Refusal> {
        let flow = frame.flow;
        let input = Input::Submit {
            flow,
            ordinal: frame.ordinal,
        };
        self.run(input, now, |ledger| {
            let mut entry = ledger.live_delivery(flow, route)?;
            if entry.submitted.frames.checked_add(1) != Some(frame.ordinal) {
                return Err(Refusal::SubmissionOrder);
            }
            entry.submitted = entry.submitted.add(Counts::new(1, frame.encoded_bytes));
            ledger.replace(flow.slot, Some(entry));
            Ok(())
        })
    }
}
