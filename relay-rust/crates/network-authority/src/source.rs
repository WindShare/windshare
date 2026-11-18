use anyhow::{Context, Result};
use std::net::IpAddr;

/// Source limits belong to the original client only when the TLS ingress is
/// explicitly trusted. Cloudflare overwrites the peer address before this check.
pub fn source_key(peer: Option<&str>, forwarded: Option<&str>, trusted: &str) -> Result<String> {
    let Some(peer) = peer else {
        return Ok("development".into());
    };
    let peer: IpAddr = peer.parse().context("invalid Cloudflare source address")?;
    let peer = peer.to_canonical();
    let trusted = trusted
        .split(',')
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::parse::<IpAddr>)
        .collect::<Result<Vec<_>, _>>()
        .context("invalid trusted proxy address")?;
    if trusted
        .into_iter()
        .any(|address| address.to_canonical() == peer)
    {
        let source: IpAddr = forwarded
            .context("trusted proxy omitted client address")?
            .parse()
            .context("invalid proxied client address")?;
        Ok(source.to_canonical().to_string())
    } else {
        Ok(peer.to_string())
    }
}

#[cfg(test)]
#[path = "source_tests.rs"]
mod tests;
