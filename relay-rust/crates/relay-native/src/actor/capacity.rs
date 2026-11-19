use super::{Actor, Environment, Persistence};
use windshare_relay_core::sessions::CapacityWait;

impl<S: Persistence, E: Environment> Actor<S, E> {
    pub(super) fn report_capacity(&mut self) {
        let now = tokio::time::Instant::now();
        let updates: Vec<_> = self
            .ports
            .values_mut()
            .flat_map(|port| port.capacity_updates(now))
            .collect();
        for (flow, milliseconds) in updates {
            let notice = CapacityWait {
                session: flow.session().wire_id(),
                direction: flow.direction(),
                milliseconds,
            };
            let source = flow.permit().source();
            let destination = flow.destination();
            let queue_notice = |connection| {
                self.ports
                    .get(&connection)
                    .is_some_and(|port| port.capacity_wait(notice).is_ok())
            };
            let source_queued = queue_notice(source);
            let destination_queued = queue_notice(destination);
            tracing::debug!(event = "relay_capacity_wait", share = ?self.identity.share,
                session = ?notice.session, direction = ?notice.direction, milliseconds,
                ?source, ?destination, source_queued, destination_queued);
        }
    }
}
