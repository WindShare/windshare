use super::config::Limits;
use std::{collections::HashMap, net::IpAddr, time::Duration};
use tokio::time::Instant;

const WINDOW: Duration = Duration::from_secs(1);

#[derive(Debug, PartialEq, Eq)]
pub(super) enum Refusal {
    ListenerRate,
    SourceRate,
    SourceCapacity,
}

pub(super) struct Limiter {
    limits: Limits,
    window: Instant,
    admitted: u32,
    sources: HashMap<IpAddr, u32>,
}

impl Limiter {
    pub(super) fn new(limits: Limits, now: Instant) -> Self {
        Self {
            limits,
            window: now,
            admitted: 0,
            sources: HashMap::new(),
        }
    }

    pub(super) fn admit(&mut self, source: IpAddr, now: Instant) -> Result<(), Refusal> {
        // Time is injected; a regressed observation cannot renew a spent budget.
        if now.saturating_duration_since(self.window) >= WINDOW {
            self.window = now;
            self.admitted = 0;
            self.sources.clear();
        }
        if self.admitted >= self.limits.requests_per_second.get() {
            return Err(Refusal::ListenerRate);
        }
        // IPv4-mapped IPv6 addresses and changed source ports share the IP budget.
        let source = source.to_canonical();
        if !self.sources.contains_key(&source)
            && self.sources.len() >= self.limits.max_sources.get()
        {
            return Err(Refusal::SourceCapacity);
        }
        let count = self.sources.entry(source).or_default();
        if *count >= self.limits.source_requests_per_second.get() {
            return Err(Refusal::SourceRate);
        }
        self.admitted += 1;
        *count += 1;
        Ok(())
    }
}

#[cfg(test)]
#[path = "limits_tests.rs"]
mod tests;
