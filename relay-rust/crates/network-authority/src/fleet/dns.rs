use serde::Deserialize;
use std::{fmt, net::IpAddr};

#[derive(Debug, PartialEq, Eq)]
pub enum DnsError {
    InvalidReply(String),
    NonPublicAddress(IpAddr),
}
impl fmt::Display for DnsError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidReply(detail) => f.write_str(detail),
            Self::NonPublicAddress(address) => write!(f, "non-public node address {address}"),
        }
    }
}
impl std::error::Error for DnsError {}

#[derive(Deserialize)]
struct Reply {
    #[serde(rename = "Status")]
    status: u8,
    #[serde(rename = "TC")]
    truncated: bool,
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
            let [a, b, c, _] = ip.octets();
            !ip.is_private()
                && !ip.is_loopback()
                && !ip.is_link_local()
                && !ip.is_documentation()
                && a != 0
                && a < 224
                && !(a == 100 && (64..128).contains(&b))
                && !(a == 198 && (18..=19).contains(&b))
                && !matches!((a, b, c), (192, 0, 0) | (192, 88, 99))
        }
        IpAddr::V6(ip) => {
            let [first, second, ..] = ip.segments();
            first >> 13 == 1
                && first != 0x2002
                && !(first == 0x2001 && (second == 0xdb8 || second < 0x200))
                && !(first == 0x3fff && second >> 12 == 0)
        }
    }
}

pub fn public_answers(bytes: &[u8], kind: u16) -> Result<usize, DnsError> {
    let reply: Reply = serde_json::from_slice(bytes)
        .map_err(|error| DnsError::InvalidReply(format!("invalid DNS JSON reply: {error}")))?;
    if reply.truncated || reply.status != 0 {
        return Err(DnsError::InvalidReply(format!(
            "incomplete DNS answer (status {}, truncated {})",
            reply.status, reply.truncated
        )));
    }
    let mut count = 0;
    for entry in reply
        .answers
        .into_iter()
        .filter(|a| matches!(a.kind, 1 | 28))
    {
        let address: IpAddr = entry.data.parse().map_err(|error| {
            DnsError::InvalidReply(format!("invalid DNS address record: {error}"))
        })?;
        if !matches!(
            (entry.kind, address),
            (1, IpAddr::V4(_)) | (28, IpAddr::V6(_))
        ) {
            return Err(DnsError::InvalidReply("DNS address family mismatch".into()));
        }
        if !public(address) {
            return Err(DnsError::NonPublicAddress(address));
        }
        count += usize::from(entry.kind == kind);
    }
    Ok(count)
}

#[cfg(test)]
#[path = "dns_tests.rs"]
mod tests;
