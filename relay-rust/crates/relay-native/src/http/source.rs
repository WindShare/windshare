use anyhow::{Context, Result, ensure};
use hyper::{HeaderMap, header::HeaderName};
use std::{collections::BTreeSet, net::IpAddr};

/// Forwarded identity requires both a trusted socket peer and one authoritative header.
#[derive(Clone, Debug, Default)]
pub struct SourcePolicy {
    proxy: Option<TrustedProxy>,
}

#[derive(Clone, Debug)]
struct TrustedProxy {
    peers: BTreeSet<IpAddr>,
    header: HeaderName,
}

impl SourcePolicy {
    /// # Errors
    /// Requires explicit proxy IPs and a valid single-IP header name.
    pub fn trusted_proxy(peers: impl IntoIterator<Item = IpAddr>, header: &str) -> Result<Self> {
        let peers: BTreeSet<_> = peers.into_iter().map(|peer| peer.to_canonical()).collect();
        ensure!(!peers.is_empty(), "trusted proxy IPs required");
        let header =
            HeaderName::from_bytes(header.as_bytes()).context("invalid source IP header")?;
        Ok(Self {
            proxy: Some(TrustedProxy { peers, header }),
        })
    }

    pub(super) fn resolve(&self, peer: IpAddr, headers: &HeaderMap) -> Result<IpAddr> {
        let peer = peer.to_canonical();
        let Some(proxy) = self
            .proxy
            .as_ref()
            .filter(|proxy| proxy.peers.contains(&peer))
        else {
            return Ok(peer);
        };
        let mut values = headers.get_all(&proxy.header).iter();
        let value = values.next().context("trusted proxy source IP missing")?;
        ensure!(values.next().is_none(), "repeated trusted proxy source IP");
        let source: IpAddr = value
            .to_str()?
            .parse()
            .context("invalid trusted proxy source IP")?;
        Ok(source.to_canonical())
    }
}

#[cfg(test)]
#[path = "source_tests.rs"]
mod tests;
