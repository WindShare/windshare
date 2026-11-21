use super::edge::{DiscoveryRejection, fetch_public};
use crate::{
    DiscoveryFailure,
    fleet::dns::{DnsError, public_answers},
};
use url::Url;

const RESOLVER: &str = "https://cloudflare-dns.com/dns-query";
const MAX_DNS_BYTES: usize = 16 * 1024;

async fn addresses(host: &str, kind: u16) -> Result<usize, DiscoveryRejection> {
    let mut url = Url::parse(RESOLVER).map_err(|error| {
        DiscoveryRejection::new(
            DiscoveryFailure::DnsFailed,
            format!("DNS resolver URL error: {error}"),
        )
    })?;
    url.query_pairs_mut()
        .append_pair("name", host)
        .append_pair("type", &kind.to_string());
    let bytes = fetch_public(url.as_str(), MAX_DNS_BYTES, "application/dns-json")
        .await
        .map_err(|error| {
            DiscoveryRejection::new(
                DiscoveryFailure::DnsFailed,
                format!("DNS query failed for {host} type {kind}: {error}"),
            )
        })?;
    public_answers(&bytes, kind).map_err(|error| match error {
        DnsError::NonPublicAddress(address) => DiscoveryRejection::new(
            DiscoveryFailure::NonPublicAddress,
            format!("DNS validation for {host} type {kind}: non-public address {address}"),
        ),
        DnsError::InvalidReply(detail) => DiscoveryRejection::new(
            DiscoveryFailure::DnsFailed,
            format!("DNS validation for {host} type {kind}: {detail}"),
        ),
    })
}

pub(super) async fn validate(origin: &str) -> Result<(), DiscoveryRejection> {
    let url = Url::parse(origin).map_err(|error| {
        DiscoveryRejection::new(
            DiscoveryFailure::InvalidUrl,
            format!("invalid origin URL: {error}"),
        )
    })?;
    let host = url.host_str().ok_or_else(|| {
        DiscoveryRejection::new(DiscoveryFailure::InvalidUrl, "missing node hostname")
    })?;
    // The descriptor fetch also uses the Worker's public Internet egress and normal
    // hostname TLS verification. No service binding or trusted network is exposed.
    let (v4, v6) = futures_util::try_join!(addresses(host, 1), addresses(host, 28))?;
    if v4 + v6 == 0 {
        return Err(DiscoveryRejection::new(
            DiscoveryFailure::DnsFailed,
            format!("no public DNS A/AAAA records for {host}"),
        ));
    }
    Ok(())
}
