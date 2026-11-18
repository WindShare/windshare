use super::{Actor, Environment, Persistence};
use anyhow::Result;
use windshare_relay_core::sessions::CapacityWait;

impl<S: Persistence, E: Environment> Actor<S, E> {
    pub(super) fn report_capacity(&mut self) -> Result<()> {
        let now = tokio::time::Instant::now();
        let updates: Vec<_> = self
            .ports
            .values_mut()
            .flat_map(|port| port.capacity_updates(now))
            .collect();
        for (flow, milliseconds) in updates {
            let bytes = CapacityWait {
                session: flow.session().wire_id(),
                direction: flow.direction(),
                milliseconds,
            }
            .encode()?;
            tracing::debug!(event = "relay_capacity_wait", share = ?self.identity.share,
                session = ?flow.session().wire_id(), direction = ?flow.direction(), milliseconds);
            for connection in [flow.permit().source(), flow.destination()] {
                if let Some(port) = self.ports.get(&connection) {
                    // Advisory pressure may be dropped under control pressure; its
                    // short lease then expires. It must never disconnect a transfer.
                    let _ = port.control(bytes.to_vec());
                }
            }
        }
        Ok(())
    }
}
