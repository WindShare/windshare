use super::Limits;
use crate::node::{
    Runtime, Settings,
    access::{Bearer, Service},
    usage::Meter,
};
use crate::storage::Persistence;
use anyhow::{Result, anyhow};
use std::{
    io,
    sync::{Arc, OnceLock, atomic::Ordering},
    task::Poll,
};
use windshare_relay_access::Attribution;
use windshare_relay_core::identifiers::ShareId;

#[derive(Clone)]
pub struct WriteOrigin {
    pub(crate) share: ShareId,
    pub(crate) bearer: Option<Bearer>,
    pub(crate) usage: Meter,
    pub(crate) share_attribution: Arc<OnceLock<Attribution>>,
}

impl WriteOrigin {
    fn limits(&self, settings: &Settings, access: &Service) -> Limits {
        Limits {
            monthly: settings.relay_budget.map(|budget| budget.bytes),
            public_daily: settings.contribution.daily_bytes.filter(|_| {
                access.egress_owner(
                    self.share,
                    self.bearer.as_ref(),
                    self.usage
                        .attribution()
                        .or_else(|| self.share_attribution.get().copied()),
                ) != Some(true)
            }),
        }
    }
}

pub(in crate::node) enum WriteAttempt {
    Socket(Poll<io::Result<usize>>),
    Refill,
}

impl Runtime {
    pub(crate) async fn public_budget_refuses(
        &self,
        share: ShareId,
        bearer: Option<&Bearer>,
    ) -> Result<bool> {
        if !self.has_budgets.load(Ordering::Acquire) {
            return Ok(false);
        }
        let store = {
            let state = self.state.lock().expect("node state");
            if !state
                .budgets
                .public_exhausted(&state.settings, crate::node::now())
            {
                return Ok(false);
            }
            if let Some(owner) = self.access.egress_owner(share, bearer, None) {
                return Ok(!owner);
            }
            state
                .control
                .clone()
                .ok_or_else(|| anyhow!("missing public budget ledger"))?
        };
        // Only exhausted, unattributed requests need durable evidence. A socket
        // may arrive before a restarted owner actor has restored its residency.
        let stored = tokio::task::spawn_blocking(move || {
            Ok::<_, anyhow::Error>(
                store
                    .load(share)?
                    .and_then(|authority| authority.record().live_attribution()),
            )
        })
        .await??;
        let state = self.state.lock().expect("node state");
        Ok(state
            .budgets
            .public_exhausted(&state.settings, crate::node::now())
            && self.access.egress_owner(share, bearer, stored) != Some(true))
    }

    pub(crate) fn public_budget_refuses_attribution(&self, attribution: Attribution) -> bool {
        if !self.has_budgets.load(Ordering::Acquire) {
            return false;
        }
        let exhausted = {
            let state = self.state.lock().expect("node state");
            state
                .budgets
                .public_exhausted(&state.settings, crate::node::now())
        };
        exhausted && !self.access.is_owner(attribution)
    }

    pub(in crate::node) fn write_egress(
        &self,
        connection_epoch: u64,
        origin: &WriteOrigin,
        requested: usize,
        timestamp: i64,
        poll_socket: impl FnOnce(usize) -> Poll<io::Result<usize>>,
    ) -> io::Result<WriteAttempt> {
        if self.unavailable.load(Ordering::Acquire) || self.epoch() != connection_epoch {
            return Err(io::Error::new(
                io::ErrorKind::ConnectionAborted,
                "relay temporarily unavailable",
            ));
        }
        if requested == 0 {
            return Ok(WriteAttempt::Socket(Poll::Ready(Ok(0))));
        }
        if !self.has_budgets.load(Ordering::Acquire) {
            return Ok(WriteAttempt::Socket(poll_socket(requested)));
        }
        let mut state = self.state.lock().expect("node state");
        if state.unavailable || self.epoch() != connection_epoch {
            return Err(io::Error::new(
                io::ErrorKind::ConnectionAborted,
                "relay temporarily unavailable",
            ));
        }
        let limits = origin.limits(&state.settings, &self.access);
        if limits.monthly.is_none() && limits.public_daily.is_none() {
            drop(state);
            return Ok(WriteAttempt::Socket(poll_socket(requested)));
        }
        if limits.public_daily.is_some() && state.budgets.public_day_elapsed(timestamp) {
            // A pending write can beat the maintenance tick at UTC midnight.
            return Ok(WriteAttempt::Refill);
        }
        if state.budgets.node_remaining(&state.settings) == 0 {
            self.revoke(&mut state, "budget_exhausted");
            return Err(io::Error::new(
                io::ErrorKind::ConnectionAborted,
                "relay budget exhausted",
            ));
        }
        if limits.public_daily.is_some() && state.budgets.public_remaining(&state.settings) == 0 {
            tracing::debug!(event = "public_relay_write_refused", share = ?origin.share);
            return Err(io::Error::new(
                io::ErrorKind::ConnectionAborted,
                "public daily relay budget exhausted",
            ));
        }
        let ready = state.budgets.ready(limits);
        if ready == 0 {
            return Ok(WriteAttempt::Refill);
        }
        // One nonblocking socket poll and its debit share a critical section.
        // A slow receiver returns Pending without holding bytes needed by another writer.
        let result = poll_socket(requested.min(usize::try_from(ready).unwrap_or(usize::MAX)));
        let mut public_exhausted = None;
        if let Poll::Ready(Ok(written)) = result {
            state.budgets.charge(limits, written as u64);
            let monthly_exhausted =
                limits.monthly.is_some() && state.budgets.node_remaining(&state.settings) == 0;
            if limits.public_daily.is_some() && state.budgets.public_remaining(&state.settings) == 0
            {
                public_exhausted = limits.public_daily;
            }
            if public_exhausted.is_some() || monthly_exhausted {
                self.bump_descriptor_revision();
            }
        }
        drop(state);
        if let Some(daily_bytes) = public_exhausted {
            tracing::info!(event = "public_relay_budget_exhausted", share = ?origin.share, daily_bytes);
            // Wake active actors so idle public shares disconnect without revoking owners.
            self.access.notify_policy_changed();
        }
        Ok(WriteAttempt::Socket(result))
    }

    pub(in crate::node) fn refill_budgets(
        &self,
        origin: &WriteOrigin,
        requested: u64,
        timestamp: i64,
    ) -> Result<()> {
        let _writer = self.lock_control()?;
        let mut state = self
            .state
            .lock()
            .map_err(|_| anyhow!("node state poisoned"))?;
        let mut limits = origin.limits(&state.settings, &self.access);
        if limits.public_daily.is_some() && state.budgets.public_day_elapsed(timestamp) {
            let settings = state.settings.clone();
            drop(state);
            let generations = settings.validate()?;
            self.apply_settings(settings, &generations, timestamp)?;
            state = self.state.lock().expect("node state");
            limits = origin.limits(&state.settings, &self.access);
        }
        if state.unavailable {
            return Ok(());
        }
        let records = state.budgets.refill(limits, requested);
        if records.is_empty() {
            return Ok(());
        }
        let store = state
            .control
            .clone()
            .ok_or_else(|| anyhow!("missing persistent relay ledger"))?;
        drop(state);
        // The control writer serializes durable changes. Other sockets may spend
        // existing credit while SQLite commits the additional credit.
        let result = store.persist_budgets(records);
        let mut state = self.state.lock().expect("node state");
        if let Err(error) = result {
            // A public-only storage failure must leave an owner able to fund the
            // monthly ledger independently. No credit from a failed transaction is issued.
            if records.public_daily.is_none() {
                state.storage_failed = true;
                self.revoke(&mut state, "budget_storage_failure");
            }
            tracing::error!(event = "relay_budget_reservation_failed", share = ?origin.share,
                monthly = records.monthly.is_some(), public_daily = records.public_daily.is_some(), %error);
            return Err(error);
        }
        state.budgets.funded(records);
        drop(state);
        Ok(())
    }
}
