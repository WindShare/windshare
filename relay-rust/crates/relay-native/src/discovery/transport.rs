use anyhow::{Context, Result, ensure};
use reqwest::{Client, Response, redirect::Policy};
use std::net::{IpAddr, SocketAddr};
use std::time::Duration;
use url::Url;

pub(super) const TIMEOUT: Duration = Duration::from_secs(4);
const MAX_CANDIDATES_PER_FAMILY: usize = 4;
const MAX_DIAL_CANDIDATES: usize = 8;
// Reject special-use and internal DNS suffixes before queueing probes so discovery
// workers never waste time on mDNS or local resolver lookups.
const RESERVED_DNS_SUFFIXES: [&str; 6] =
    ["localhost", "local", "internal", "invalid", "onion", "arpa"];

fn is_reserved_dns_name(name: &str) -> bool {
    let trimmed = name.strip_suffix('.').unwrap_or(name);
    trimmed.rsplit('.').next().is_some_and(|label| {
        RESERVED_DNS_SUFFIXES
            .iter()
            .any(|suffix| label.eq_ignore_ascii_case(suffix))
    })
}

// Restrict IPv6 to allocated global unicast and reject transition prefixes that
// can tunnel into an IPv4 network behind the public endpoint.
pub(super) fn public(ip: IpAddr) -> bool {
    match ip.to_canonical() {
        IpAddr::V4(v) => {
            let [a, b, c, _] = v.octets();
            !(v.is_private()
                || v.is_loopback()
                || v.is_link_local()
                || v.is_documentation()
                || a == 0
                || a >= 224
                || (a == 100 && (64..128).contains(&b))
                || (a == 192 && ((b == 0 && c == 0) || (b == 88 && c == 99)))
                || (a == 198 && (b == 18 || b == 19)))
        }
        IpAddr::V6(v) => {
            let [a, b, ..] = v.segments();
            (0x2000..0x4000).contains(&a)
                && a != 0x2002
                && !(a == 0x2001 && (b < 0x200 || b == 0xdb8))
                && !(a == 0x3fff && b < 0x1000)
        }
    }
}

/// Validates a public HTTPS relay base URL, permitting valid subpath mounts
/// because `/.well-known/windshare` and `/discovery/v1/peers` live at origin root.
pub(super) fn relay_origin(raw: &str) -> Result<Url> {
    let url = Url::parse(raw)?;
    ensure!(
        url.scheme() == "https"
            && url.username().is_empty()
            && url.password().is_none()
            && url.port() != Some(0)
            && url.query().is_none()
            && url.fragment().is_none(),
        "expected HTTPS node address"
    );
    match url.host().context("missing node host")? {
        url::Host::Domain(name) => {
            crate::config::validate_dns(name)?;
            ensure!(
                name.strip_suffix('.').unwrap_or(name).contains('.') && !is_reserved_dns_name(name),
                "expected public DNS name"
            );
        }
        url::Host::Ipv4(ip) => ensure!(public(ip.into()), "non-public node address"),
        url::Host::Ipv6(ip) => ensure!(public(ip.into()), "non-public node address"),
    }
    Ok(url)
}

/// Validates a configured discovery seed URL, rejecting non-root paths while
/// still tolerating a trailing slash copied from a browser address bar.
pub(super) fn seed_origin(raw: &str) -> Result<Url> {
    let url = relay_origin(raw)?;
    ensure!(
        matches!(url.path(), "" | "/"),
        "expected HTTPS origin without path"
    );
    Ok(url)
}

/// Canonicalizes a validated node URL into its `scheme://host[:port]` origin
/// without path or trailing slash so book peers, seeds, and local endpoints
/// compare identically.
pub(super) fn canonical_origin(url: &Url) -> String {
    url.origin().ascii_serialization()
}

pub(super) fn canonical_origin_str(raw: &str) -> Result<String> {
    relay_origin(raw).map(|url| canonical_origin(&url))
}

/// Filters resolved socket addresses down to deduplicated public addresses,
/// balancing the preferred and fallback IP families so a long AAAA list cannot
/// starve IPv4 Happy Eyeballs fallback in hyper's connector.
pub(super) fn select_public_addrs(
    answers: impl IntoIterator<Item = SocketAddr>,
) -> Vec<SocketAddr> {
    let mut primaries = Vec::new();
    let mut fallbacks = Vec::new();
    let mut seen = Vec::new();
    let mut primary_is_v6 = None;

    for addr in answers {
        let ip = addr.ip().to_canonical();
        if !public(ip) {
            continue;
        }
        let canonical = SocketAddr::new(ip, addr.port());
        if seen.contains(&canonical) {
            continue;
        }
        seen.push(canonical);
        let is_v6 = ip.is_ipv6();
        let preferred = *primary_is_v6.get_or_insert(is_v6);
        let bucket = if is_v6 == preferred {
            &mut primaries
        } else {
            &mut fallbacks
        };
        if bucket.len() < MAX_CANDIDATES_PER_FAMILY {
            bucket.push(canonical);
        }
    }

    let mut out = Vec::with_capacity((primaries.len() + fallbacks.len()).min(MAX_DIAL_CANDIDATES));
    let mut index = 0;
    while out.len() < MAX_DIAL_CANDIDATES && (index < primaries.len() || index < fallbacks.len()) {
        if let Some(&addr) = primaries.get(index) {
            out.push(addr);
        }
        if out.len() < MAX_DIAL_CANDIDATES
            && let Some(&addr) = fallbacks.get(index)
        {
            out.push(addr);
        }
        index += 1;
    }
    out
}

pub(super) async fn pinned(url: &Url) -> Result<Client> {
    let host = url
        .host_str()
        .context("missing host")?
        .trim_matches(['[', ']']);
    let port = url.port_or_known_default().context("missing port")?;
    let mut builder = Client::builder()
        .no_proxy()
        .https_only(true)
        .timeout(TIMEOUT)
        .redirect(Policy::none());
    if let Ok(ip) = host.parse::<IpAddr>() {
        ensure!(public(ip), "non-public peer");
    } else {
        let answers =
            tokio::time::timeout(TIMEOUT, tokio::net::lookup_host((host, port))).await??;
        let addrs = select_public_addrs(answers);
        ensure!(!addrs.is_empty(), "no public DNS address");
        // Pin all bounded public answers so hyper runs Happy Eyeballs across
        // both address families without a second DNS lookup after validation.
        builder = builder.resolve_to_addrs(host, &addrs);
    }
    Ok(builder.build()?)
}

/// Returns the actual peer IP that served the response after Happy Eyeballs
/// selection so per-IP and per-prefix peer-book quotas reflect the real route.
pub(super) fn verified_remote_ip(response: &Response) -> Result<IpAddr> {
    let ip = response
        .remote_addr()
        .context("missing connected peer address")?
        .ip()
        .to_canonical();
    ensure!(public(ip), "non-public peer");
    Ok(ip)
}

pub(super) async fn body(response: Response, limit: usize) -> Result<Vec<u8>> {
    ensure!(
        response.status().is_success(),
        "discovery HTTP {}",
        response.status()
    );
    let mut response = response;
    let mut data = Vec::new();
    while let Some(chunk) = response.chunk().await? {
        ensure!(
            chunk.len() <= limit - data.len(),
            "oversized discovery response"
        );
        data.extend_from_slice(&chunk);
    }
    Ok(data)
}

#[cfg(test)]
#[path = "transport_tests.rs"]
mod tests;
