//! Only completed socket writes enter accounting; queues and failed writes are free.
mod outbox;
mod reporting;
mod restriction_sync;
#[cfg(test)]
#[path = "usage_tests.rs"]
mod tests;
pub use reporting::{Authority, Reporter};

use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex, OnceLock},
};
use windshare_relay_access::{AccountId, Attribution, Issuer, entitlement::DAY_SECONDS};

type Key = (Issuer, AccountId, u64);
#[derive(Clone, Default)]
pub struct Collector {
    issuer: Arc<OnceLock<Issuer>>,
    records: Arc<Mutex<BTreeMap<Key, u64>>>,
}
impl Collector {
    pub fn configure(&self, issuer: Issuer) {
        self.issuer.get_or_init(|| issuer);
    }

    pub fn is_configured(&self) -> bool {
        self.issuer.get().is_some()
    }

    pub fn record(&self, attribution: Attribution, bytes: usize, timestamp: u64) {
        if let Attribution::Account {
            issuer, account, ..
        } = attribution
        {
            // Accepting an external issuer does not make a self-hosted node its billing agent,
            // and synthetic probe traffic is never billable usage.
            if self.issuer.get() != Some(&issuer)
                || account == windshare_relay_access::probe::PROBE_ACCOUNT
                || bytes == 0
            {
                return;
            }
            let mut records = self.records.lock().expect("usage collector");
            let total = records
                .entry((issuer, account, timestamp / DAY_SECONDS))
                .or_default();
            *total = total.saturating_add(bytes as u64);
            drop(records);
        }
    }

    fn drain(&self) -> BTreeMap<Key, u64> {
        std::mem::take(&mut *self.records.lock().expect("usage collector"))
    }

    fn restore(&self, records: BTreeMap<Key, u64>) {
        let mut pending = self.records.lock().expect("usage collector");
        for (key, bytes) in records {
            let value = pending.entry(key).or_default();
            *value = value.saturating_add(bytes);
        }
        drop(pending);
    }
}

/// One writer owns this scope. The read half can flush WebSocket control frames,
/// but cannot replace the attribution of the opaque frame being written.
#[derive(Clone, Default)]
pub struct Meter(Arc<Mutex<Option<Attribution>>>);

pub struct Scope(Meter);
impl Meter {
    pub fn enter(&self, attribution: Attribution) -> Scope {
        *self.0.lock().expect("usage scope") = Some(attribution);
        Scope(self.clone())
    }

    pub(crate) fn attribution(&self) -> Option<Attribution> {
        *self.0.lock().expect("usage scope")
    }

    pub fn record(&self, collector: &Collector, bytes: usize, timestamp: u64) {
        let attribution = self.attribution();
        if let Some(attribution) = attribution {
            collector.record(attribution, bytes, timestamp);
        }
    }
}
impl Drop for Scope {
    fn drop(&mut self) {
        *self.0.0.lock().expect("usage scope") = None;
    }
}
