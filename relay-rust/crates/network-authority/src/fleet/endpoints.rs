use crate::Fault;
use anyhow::{Result, ensure};
use percent_encoding::percent_decode_str;
use url::{Host, Url};

fn valid_domain(domain: &str) -> bool {
    let name = domain.strip_suffix('.').unwrap_or(domain);
    name.len() <= 253
        && name.split('.').all(|label| {
            !label.is_empty()
                && label.len() <= 63
                && !label.starts_with('-')
                && !label.ends_with('-')
                && (label.as_bytes().get(2..4) != Some(b"--") || label.starts_with("xn--"))
                && label
                    .bytes()
                    .all(|c| c.is_ascii_alphanumeric() || c == b'-')
        })
}

fn base(raw: &str) -> Result<Url> {
    let authority = raw.strip_prefix("https://").ok_or(Fault::Invalid)?;
    let (authority, path) = authority.split_once('/').unwrap_or((authority, ""));
    ensure!(
        !authority.is_empty()
            && !authority.contains('@')
            && !raw.contains(['?', '#', '\\'])
            && !raw.bytes().any(|byte| byte.is_ascii_control())
            && raw.trim_matches(|c: char| c.is_whitespace() || c == '\u{feff}') == raw,
        Fault::Invalid
    );
    // URL parsers erase dot segments before exposing the path. The signed spelling must pass first.
    for segment in path.split('/') {
        for (index, _) in segment.match_indices('%') {
            ensure!(
                segment
                    .as_bytes()
                    .get(index + 1..index + 3)
                    .is_some_and(|s| s.iter().all(u8::is_ascii_hexdigit)),
                Fault::Invalid
            );
        }
        let decoded = percent_decode_str(segment)
            .decode_utf8()
            .map_err(|_| Fault::Invalid)?;
        ensure!(!matches!(decoded.as_ref(), "." | ".."), Fault::Invalid);
    }
    let mut url = Url::parse(raw).map_err(|_| Fault::Invalid)?;
    ensure!(url.port() != Some(0), Fault::Invalid);
    match url.host().ok_or(Fault::Invalid)? {
        Host::Domain(domain) => ensure!(valid_domain(domain), Fault::Invalid),
        Host::Ipv4(ip) => ensure!(
            authority.split(':').next() == Some(ip.to_string().as_str()),
            Fault::Invalid
        ),
        Host::Ipv6(ip) => {
            if let Some(v4) = ip.to_ipv4_mapped() {
                url.set_ip_host(v4.into()).map_err(|()| Fault::Invalid)?;
            }
        }
    }
    Ok(url)
}

/// # Errors
/// Rejects non-origin inputs and hosts outside public DNS enrollment.
pub fn public_origin(raw: &str) -> Result<String> {
    let url = base(raw)?;
    let Some(Host::Domain(domain)) = url.host() else {
        return Err(Fault::Invalid.into());
    };
    ensure!(
        url.path() == "/"
            && domain.contains('.')
            && !domain.ends_with('.')
            && !["localhost", "local", "internal", "invalid", "onion", "arpa"]
                .contains(&domain.rsplit('.').next().unwrap_or("")),
        Fault::Invalid
    );
    Ok(url.origin().ascii_serialization())
}

pub(super) fn relay_identity(raw: &str) -> Result<String> {
    let url = base(raw)?;
    let address = &url[url::Position::BeforeHost..];
    Ok(format!(
        "wss://{}/v2/ws",
        address.strip_suffix('/').unwrap_or(address)
    ))
}

/// # Errors
/// STUN entries must already be spelled as their canonical host and nonzero UDP port.
pub fn stun_host(raw: &str) -> Result<String> {
    let (host, port) = raw
        .strip_prefix("stun:")
        .and_then(|v| v.rsplit_once(':'))
        .ok_or(Fault::Invalid)?;
    let port = port.parse::<u16>().map_err(|_| Fault::Invalid)?;
    let url = base(&format!("https://{host}"))?;
    ensure!(
        port > 0 && Some(host) == url.host_str() && format!("stun:{host}:{port}") == raw,
        Fault::Invalid
    );
    Ok(host.to_owned())
}
