use super::record::{Envelope, Record};
use std::{collections::BTreeMap, net::IpAddr};

pub(super) const MAX_PEERS: usize = 128;
const PER_IP: usize = 2;
const PER_PREFIX: usize = 4;
const ANNOUNCEMENTS_PER_MINUTE: usize = 64;
const PER_SOURCE: usize = 4;
pub(super) const SAMPLE: usize = 8;
struct Peer {
    record: Record,
    ip: IpAddr,
    since: u64,
}

#[derive(Default)]
pub(super) struct Book {
    peers: BTreeMap<String, Peer>,
    cursor: usize,
    minute: u64,
    sources: BTreeMap<IpAddr, usize>,
    received: usize,
}

pub(super) fn prefix(ip: IpAddr) -> u128 {
    match ip {
        IpAddr::V4(v) => u128::from(u32::from(v) >> 8),
        IpAddr::V6(v) => u128::from(v) >> 80,
    }
}

impl Book {
    pub fn allow(&mut self, source: IpAddr, now: u64) -> bool {
        if self.minute != now / 60 {
            self.minute = now / 60;
            self.received = 0;
            self.sources.clear();
        }
        if self.received >= ANNOUNCEMENTS_PER_MINUTE {
            return false;
        }
        let count = self.sources.entry(source.to_canonical()).or_default();
        if *count >= PER_SOURCE {
            return false;
        }
        *count += 1;
        self.received += 1;
        true
    }

    pub fn insert(&mut self, record: Record, ip: IpAddr, now: u64) -> anyhow::Result<()> {
        self.peers.retain(|_, p| p.record.expires > now);
        let known = self.peers.get(&record.key);
        anyhow::ensure!(
            known.is_none_or(|p| record.issued >= p.record.issued),
            "older record"
        );
        let since = known
            .filter(|p| p.ip == ip && p.record.endpoint == record.endpoint)
            .map_or(now, |p| p.since);
        let others = self.peers.values().filter(|p| p.record.key != record.key);
        anyhow::ensure!(
            others.clone().filter(|p| p.ip == ip).count() < PER_IP
                && others
                    .filter(|p| p.ip.is_ipv4() == ip.is_ipv4() && prefix(p.ip) == prefix(ip))
                    .count()
                    < PER_PREFIX,
            "address quota"
        );
        if known.is_none() && self.peers.len() == MAX_PEERS {
            let newest = self
                .peers
                .values()
                .max_by_key(|p| p.since)
                .expect("full book");
            let key = newest.record.key.clone();
            tracing::debug!(event="discovery_peer_evicted", node_key=%key, reason="capacity");
            self.peers.remove(&key);
        }
        self.peers
            .insert(record.key.clone(), Peer { record, ip, since });
        Ok(())
    }

    pub fn sample(&mut self, now: u64, limit: usize) -> Vec<Envelope> {
        self.peers.retain(|_, p| p.record.expires > now);
        let n = self.peers.len();
        if n == 0 || limit == 0 {
            return Vec::new();
        }
        let start = self.cursor % n;
        let result: Vec<_> = self
            .peers
            .values()
            .cycle()
            .skip(start)
            .take(n.min(limit))
            .map(|p| p.record.envelope.clone())
            .collect();
        // Advance by the returned slice so the caller's slot budget cannot skip peers across turns.
        self.cursor = (start + result.len()) % n;
        result
    }

    pub fn records(&self, now: u64) -> Vec<Envelope> {
        self.peers
            .values()
            .filter(|peer| peer.record.expires > now)
            .map(|peer| peer.record.envelope.clone())
            .collect()
    }

    pub fn targets(&self, now: u64) -> Vec<String> {
        self.peers
            .values()
            .filter(|p| p.record.expires > now)
            .map(|p| p.record.origin.to_string())
            .collect()
    }
}
