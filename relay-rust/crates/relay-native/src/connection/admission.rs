//! Process capacity stays with the socket and every task that can retain its resources.
use anyhow::{Result, anyhow, ensure};
use std::sync::{Arc, Mutex};
use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use windshare_relay_core::{
    clock::UnixMillis,
    handshake::Initiation,
    lifecycle::ConnectionRef,
    share::{AttemptLimiter, AttemptRate},
};

pub struct Capacity {
    business: Arc<Semaphore>,
    controls: Arc<Semaphore>,
    attempts: Mutex<AttemptLimiter>,
}

enum Slot {
    Control { _permit: OwnedSemaphorePermit },
    Business { _permit: OwnedSemaphorePermit },
}

pub struct Admission {
    capacity: Arc<Capacity>,
    slot: Mutex<Slot>,
}

impl Capacity {
    pub fn new(
        business: usize,
        controls: usize,
        attempts: AttemptRate,
        now: UnixMillis,
    ) -> Result<Arc<Self>> {
        ensure!(
            business != 0 && controls != 0,
            "positive connection capacity required"
        );
        Ok(Arc::new(Self {
            business: Arc::new(Semaphore::new(business)),
            controls: Arc::new(Semaphore::new(controls)),
            attempts: Mutex::new(
                AttemptLimiter::new(attempts, now)
                    .map_err(|error| anyhow!("invalid ingress rate: {error:?}"))?,
            ),
        }))
    }

    pub fn accept(self: &Arc<Self>, now: UnixMillis) -> Result<Arc<Admission>> {
        let permit = Arc::clone(&self.controls)
            .try_acquire_owned()
            .map_err(|_| anyhow!("control connection capacity exhausted"))?;
        ensure!(
            self.attempts.lock().expect("admission lock").attempt(now),
            "connection attempt rate exhausted"
        );
        Ok(Arc::new(Admission {
            capacity: Arc::clone(self),
            slot: Mutex::new(Slot::Control { _permit: permit }),
        }))
    }
}

impl Admission {
    pub fn inspect(&self, frame: &[u8], connection: ConnectionRef) -> Result<()> {
        // Parsing here reserves resources only. The kernel remains the sole
        // authority for authentication, phase ordering and per-share admission.
        if !Initiation::decode(frame).is_ok_and(|intent| intent.uses_business_capacity()) {
            return Ok(());
        }
        let mut slot = self.slot.lock().expect("admission lock");
        if matches!(*slot, Slot::Business { .. }) {
            return Ok(());
        }
        let permit = Arc::clone(&self.capacity.business)
            .try_acquire_owned()
            .map_err(|_| {
                tracing::debug!(event = "business_connection_capacity", ?connection);
                anyhow!("business connection capacity exhausted")
            })?;
        *slot = Slot::Business { _permit: permit };
        tracing::debug!(event = "business_connection_admitted", ?connection);
        Ok(())
    }
}

#[cfg(test)]
#[path = "admission_tests.rs"]
mod tests;
