//! Share-level retirement coordination: a detached source can still own
//! unsubmitted queues on sibling destinations until discard confirmation.
use super::{
    ConnectionRef, ConnectionUsage, Observer, RelayDecision, RetirementPolicy, ShareRelay,
};
use crate::clock::UnixMillis;
use alloc::vec::Vec;

impl<O: Observer> ShareRelay<O> {
    pub fn connection_usage(&self) -> ConnectionUsage {
        self.generations.usage()
    }

    pub(super) fn generation_quiescent(&self, connection: ConnectionRef) -> bool {
        if self.sessions.entries().values().any(|entry| {
            let key = entry.session.key();
            (key.sender() == connection || key.receiver() == connection)
                && (entry.deferred || entry.session.phase() != crate::sessions::Phase::Retired)
        }) {
            return false;
        }
        // The lost peer can also source payloads queued on healthy destinations.
        self.sessions.routes().values().all(|flow| {
            (flow.destination() != connection && flow.permit().source() != connection)
                || self
                    .generations
                    .ledger(&flow.destination())
                    .and_then(|ledger| ledger.snapshot(*flow))
                    .is_none_or(|snapshot| snapshot.phase == crate::flow::Phase::Settling)
        })
    }

    pub(super) fn progress_retirements(&mut self, now: UnixMillis) {
        let released: Vec<_> = self.generations.released().collect();
        for connection in released {
            self.collect_ledger(connection, now);
        }
        if self.limits.retirement == RetirementPolicy::AfterDiscard {
            let detached: Vec<_> = self.generations.detached_ids().collect();
            for connection in detached {
                if self.generation_quiescent(connection)
                    && self.generations.release_after_discard(connection)
                {
                    self.sessions.release_destination(connection);
                    self.trace(Some(connection), RelayDecision::ResponsibilityReleased, now);
                }
            }
        }
    }
}
