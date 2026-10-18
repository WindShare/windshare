use super::config::MAX_DATAGRAM_BYTES;
use std::net::SocketAddr;

const HEADER_BYTES: usize = 20;
const ATTRIBUTE_HEADER_BYTES: usize = 4;
const ALIGNMENT: usize = 4;
const MAGIC_COOKIE: [u8; 4] = [0x21, 0x12, 0xa4, 0x42];
const PORT_MASK: u16 = 0x2112;
const BINDING_REQUEST: u16 = 0x0001;
const BINDING_SUCCESS: u16 = 0x0101;
const BINDING_ERROR: u16 = 0x0111;
const XOR_MAPPED_ADDRESS: u16 = 0x0020;
const MAPPED_ADDRESS: u16 = 0x0001;
const USERNAME: u16 = 0x0006;
const MESSAGE_INTEGRITY: u16 = 0x0008;
const REALM: u16 = 0x0014;
const NONCE: u16 = 0x0015;
const MESSAGE_INTEGRITY_SHA256: u16 = 0x001c;
const PASSWORD_ALGORITHM: u16 = 0x001d;
const USERHASH: u16 = 0x001e;
const ERROR_CODE: u16 = 0x0009;
const UNKNOWN_ATTRIBUTES: u16 = 0x000a;
const FINGERPRINT: u16 = 0x8028;
const FINGERPRINT_MASK: u32 = 0x5354_554e;
const OPTIONAL_ATTRIBUTE: u16 = 0x8000;
const IPV4_FAMILY: u8 = 1;
const IPV6_FAMILY: u8 = 2;

#[derive(Debug, PartialEq, Eq)]
pub(super) enum Invalid {
    Size,
    Header,
    Attribute,
    Fingerprint,
}

/// Stateless UDP Binding profile: no credentials, transaction cache, or redirects.
/// Unsupported required extensions receive 420 instead of silently succeeding.
pub(super) fn binding_response(data: &[u8], remote: SocketAddr) -> Result<Vec<u8>, Invalid> {
    if !(HEADER_BYTES..=MAX_DATAGRAM_BYTES).contains(&data.len()) {
        return Err(Invalid::Size);
    }
    if read_u16(data) != BINDING_REQUEST
        || data[4..8] != MAGIC_COOKIE
        || usize::from(read_u16(&data[2..])) + HEADER_BYTES != data.len()
        || !data.len().is_multiple_of(ALIGNMENT)
    {
        return Err(Invalid::Header);
    }
    let unknown = attributes(data)?;
    let mut response = data[..HEADER_BYTES].to_vec();
    if unknown.is_empty() {
        response[..2].copy_from_slice(&BINDING_SUCCESS.to_be_bytes());
        let mut address = vec![
            0,
            if remote.ip().to_canonical().is_ipv4() {
                IPV4_FAMILY
            } else {
                IPV6_FAMILY
            },
        ];
        address.extend_from_slice(&(remote.port() ^ PORT_MASK).to_be_bytes());
        let octets = match remote.ip().to_canonical() {
            std::net::IpAddr::V4(ip) => ip.octets().to_vec(),
            std::net::IpAddr::V6(ip) => ip.octets().to_vec(),
        };
        address.extend(
            octets
                .iter()
                .zip(&data[4..HEADER_BYTES])
                .map(|(byte, mask)| byte ^ mask),
        );
        append_attribute(&mut response, XOR_MAPPED_ADDRESS, &address);
    } else {
        response[..2].copy_from_slice(&BINDING_ERROR.to_be_bytes());
        // An empty reason phrase keeps the unauthenticated error reply small.
        append_attribute(&mut response, ERROR_CODE, &[0, 0, 4, 20]);
        let unknown: Vec<u8> = unknown.iter().flat_map(|kind| kind.to_be_bytes()).collect();
        append_attribute(&mut response, UNKNOWN_ATTRIBUTES, &unknown);
    }
    let length = u16::try_from(response.len() + ATTRIBUTE_HEADER_BYTES + ALIGNMENT - HEADER_BYTES)
        .expect("bounded STUN response");
    response[2..4].copy_from_slice(&length.to_be_bytes());
    let fingerprint = fingerprint(&response).to_be_bytes();
    append_attribute(&mut response, FINGERPRINT, &fingerprint);
    Ok(response)
}

fn attributes(data: &[u8]) -> Result<Vec<u16>, Invalid> {
    let mut unknown = Vec::new();
    let mut offset = HEADER_BYTES;
    while offset < data.len() {
        let header = data
            .get(offset..offset + ATTRIBUTE_HEADER_BYTES)
            .ok_or(Invalid::Attribute)?;
        let kind = read_u16(header);
        let length = usize::from(read_u16(&header[2..]));
        let value_start = offset + ATTRIBUTE_HEADER_BYTES;
        let end = value_start + length.next_multiple_of(ALIGNMENT);
        if end > data.len() {
            return Err(Invalid::Attribute);
        }
        if kind == FINGERPRINT {
            if length != ALIGNMENT
                || end != data.len()
                || data[value_start..end] != fingerprint(&data[..offset]).to_be_bytes()
            {
                return Err(Invalid::Fingerprint);
            }
        } else if kind < OPTIONAL_ATTRIBUTE && !known_base_attribute(kind) {
            unknown.push(kind);
        }
        offset = end;
    }
    unknown.sort_unstable();
    unknown.dedup();
    Ok(unknown)
}

const fn known_base_attribute(kind: u16) -> bool {
    // RFC 8489 section 6.3 permits ignoring known but unexpected attributes.
    // This unauthenticated usage neither validates credentials nor reflects them.
    matches!(
        kind,
        MAPPED_ADDRESS
            | USERNAME
            | MESSAGE_INTEGRITY
            | ERROR_CODE
            | UNKNOWN_ATTRIBUTES
            | REALM
            | NONCE
            | MESSAGE_INTEGRITY_SHA256
            | PASSWORD_ALGORITHM
            | USERHASH
            | XOR_MAPPED_ADDRESS
    )
}

fn fingerprint(data: &[u8]) -> u32 {
    crc32fast::hash(data) ^ FINGERPRINT_MASK
}

const fn read_u16(data: &[u8]) -> u16 {
    u16::from_be_bytes([data[0], data[1]])
}

fn append_attribute(data: &mut Vec<u8>, kind: u16, value: &[u8]) {
    data.extend_from_slice(&kind.to_be_bytes());
    data.extend_from_slice(
        &u16::try_from(value.len())
            .expect("bounded attribute")
            .to_be_bytes(),
    );
    data.extend_from_slice(value);
    data.resize(data.len().next_multiple_of(ALIGNMENT), 0);
}

#[cfg(test)]
#[path = "protocol_tests.rs"]
mod tests;
