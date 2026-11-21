use super::edge::fetch_public;
use anyhow::{Result, ensure};
use serde::Deserialize;
use std::net::{IpAddr, Ipv4Addr};
use url::Url;

const RESOLVER: &str = "https://cloudflare-dns.com/dns-query";
const MAX_DNS_BYTES: usize = 16 * 1024;
#[derive(Deserialize)]
struct Reply {
    #[serde(rename = "Status")]
    status: u8,
    #[serde(rename = "Answer", default)]
    answers: Vec<Answer>,
}
#[derive(Deserialize)]
struct Answer {
    #[serde(rename = "type")]
    kind: u16,
    data: String,
}

fn public(address: IpAddr) -> bool {
    match address {
        IpAddr::V4(ip) => {
            let [a, b, _, _] = ip.octets();
            !ip.is_private()
                && !ip.is_loopback()
                && !ip.is_link_local()
                && !ip.is_documentation()
                && a != 0
                && a < 224
                && !(a == 100 && (64..128).contains(&b))
                && !(a == 198 && (18..=19).contains(&b))
                && (u32::from(ip) >> 8) != (u32::from(Ipv4Addr::new(192, 0, 0, 0)) >> 8)
        }
        IpAddr::V6(ip) => {
            let [first, second, ..] = ip.segments();
            first >> 13 == 1
                && first != 0x2002
                && !(first == 0x2001 && (second == 0xdb8 || second < 0x200))
        }
    }
}

async fn addresses(host: &str, kind: u16) -> Result<usize> {
    let mut url = Url::parse(RESOLVER)?;
    url.query_pairs_mut()
        .append_pair("name", host)
        .append_pair("type", &kind.to_string());
    let bytes = fetch_public(url.as_str(), MAX_DNS_BYTES, "application/dns-json").await?;
    let reply: Reply = serde_json::from_slice(&bytes)?;
    ensure!(reply.status == 0, "DNS resolution failed");
    let mut count = 0;
    for entry in reply.answers.into_iter().filter(|a| a.kind == kind) {
        ensure!(public(entry.data.parse()?), "non-public node address");
        count += 1;
    }
    Ok(count)
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
