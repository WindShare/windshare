use anyhow::{Context, Result, ensure};
use reqwest::{Client, Response, redirect::Policy};
use std::net::{IpAddr, SocketAddr};
use std::time::Duration;
use url::Url;

pub(super) const TIMEOUT: Duration = Duration::from_secs(4);

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

pub(super) fn origin(raw: &str) -> Result<Url> {
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
        url::Host::Domain(name) => ensure!(
            name.trim_end_matches('.').contains('.'),
            "expected public DNS name"
        ),
        url::Host::Ipv4(ip) => ensure!(public(ip.into()), "non-public node address"),
        url::Host::Ipv6(ip) => ensure!(public(ip.into()), "non-public node address"),
    }
    Ok(url)
}

pub(super) async fn pinned(url: &Url) -> Result<(Client, IpAddr)> {
    let host = url
        .host_str()
        .context("missing host")?
        .trim_matches(['[', ']']);
    let port = url.port_or_known_default().context("missing port")?;
    let address = if let Ok(ip) = host.parse::<IpAddr>() {
        SocketAddr::new(ip, port)
    } else {
        let answers =
            tokio::time::timeout(TIMEOUT, tokio::net::lookup_host((host, port))).await??;
        answers
            .into_iter()
            .find(|addr| public(addr.ip()))
            .context("no public DNS address")?
    };
    ensure!(public(address.ip()), "non-public peer");
    // TLS still verifies the original name. No second DNS lookup, proxy or redirect
    // can substitute a different destination after the address check.
    let client = Client::builder()
        .no_proxy()
        .https_only(true)
        .timeout(TIMEOUT)
        .redirect(Policy::none())
        .resolve(host, address)
        .build()?;
    Ok((client, address.ip().to_canonical()))
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
