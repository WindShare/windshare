use super::edge::fetch_public;
use crate::fleet::dns::public_answers;
use anyhow::Context;
use anyhow::{Result, ensure};
use url::Url;

const RESOLVER: &str = "https://cloudflare-dns.com/dns-query";
const MAX_DNS_BYTES: usize = 16 * 1024;
async fn addresses(host: &str, kind: u16) -> Result<usize> {
    let mut url = Url::parse(RESOLVER)?;
    url.query_pairs_mut()
        .append_pair("name", host)
        .append_pair("type", &kind.to_string());
    let bytes = fetch_public(url.as_str(), MAX_DNS_BYTES, "application/dns-json").await?;
    public_answers(&bytes, kind).with_context(|| format!("DNS validation for {host} type {kind}"))
}

pub(super) async fn validate(origin: &str) -> Result<()> {
    let url = Url::parse(origin)?;
    let host = url
        .host_str()
        .ok_or_else(|| anyhow::anyhow!("missing node hostname"))?;
    // The descriptor fetch also uses the Worker's public Internet egress and normal
    // hostname TLS verification. No service binding or trusted network is exposed.
    let (v4, v6) = futures_util::try_join!(addresses(host, 1), addresses(host, 28))?;
    ensure!(v4 + v6 > 0, "no public node address");
    Ok(())
}
