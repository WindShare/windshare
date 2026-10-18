use super::*;

fn hex(value: &str) -> Vec<u8> {
    value
        .split_whitespace()
        .map(|byte| u8::from_str_radix(byte, 16).unwrap())
        .collect()
}

fn request(attributes: &[u8]) -> Vec<u8> {
    let mut data = hex("00 01 00 00 21 12 a4 42 b7 e7 a7 01 bc 34 d6 86 fa 87 df ae");
    data[2..4].copy_from_slice(&u16::try_from(attributes.len()).unwrap().to_be_bytes());
    data.extend_from_slice(attributes);
    data
}

fn remote() -> SocketAddr {
    "192.0.2.1:32853".parse().unwrap()
}

#[test]
fn binding_uses_rfc5769_xor_address_values_and_preserves_the_transaction() {
    for (remote, expected) in [
        ("192.0.2.1:32853", "00 01 a1 47 e1 12 a6 43"),
        (
            "[2001:db8:1234:5678:11:2233:4455:6677]:32853",
            "00 02 a1 47 01 13 a9 fa a5 d3 f1 79 bc 25 f4 b5 be d2 b9 d9",
        ),
        ("[::ffff:192.0.2.1]:32853", "00 01 a1 47 e1 12 a6 43"),
    ] {
        let request = request(&[]);
        let response = binding_response(&request, remote.parse().unwrap()).unwrap();
        let expected = hex(expected);
        assert_eq!(&response[..2], &BINDING_SUCCESS.to_be_bytes());
        assert_eq!(&response[8..20], &request[8..20]);
        assert_eq!(read_u16(&response[20..]), XOR_MAPPED_ADDRESS);
        assert_eq!(&response[24..24 + expected.len()], expected);
        assert_eq!(
            usize::from(read_u16(&response[2..])) + HEADER_BYTES,
            response.len()
        );
        assert_eq!(
            &response[response.len() - 4..],
            fingerprint(&response[..response.len() - 8]).to_be_bytes()
        );
    }
}

#[test]
fn fingerprint_matches_the_published_rfc5769_request() {
    // RFC 5769 section 2.1 is independent evidence for CRC coverage and byte order.
    let data = hex("00 01 00 58 21 12 a4 42 b7 e7 a7 01 bc 34 d6 86 fa 87 df ae
        80 22 00 10 53 54 55 4e 20 74 65 73 74 20 63 6c 69 65 6e 74
        00 24 00 04 6e 00 01 ff 80 29 00 08 93 2f f9 b1 51 26 3b 36
        00 06 00 09 65 76 74 6a 3a 68 36 76 59 20 20 20
        00 08 00 14 9a ea a7 0c bf d8 cb 56 78 1e f2 b5 b2 d3 f2 49 c1 b5 71 a2
        80 28 00 04 e5 7a 3b cf");
    assert_eq!(fingerprint(&data[..data.len() - 8]), 0xe57a_3bcf);
    assert!(binding_response(&data, remote()).is_ok());
    let mut corrupt = data;
    *corrupt.last_mut().unwrap() ^= 1;
    assert_eq!(
        binding_response(&corrupt, remote()),
        Err(Invalid::Fingerprint)
    );
}

#[test]
fn unsupported_required_extensions_receive_bounded_420_and_optional_padding_is_ignored() {
    let optional = request(&hex("80 22 00 01 61 ff ff ff"));
    assert_eq!(
        read_u16(&binding_response(&optional, remote()).unwrap()),
        BINDING_SUCCESS
    );
    let username = request(&hex("00 06 00 01 61 00 00 00"));
    assert_eq!(
        read_u16(&binding_response(&username, remote()).unwrap()),
        BINDING_SUCCESS
    );
    let required = request(&hex("00 24 00 00 00 03 00 00 00 24 00 00"));
    let response = binding_response(&required, remote()).unwrap();
    assert_eq!(read_u16(&response), BINDING_ERROR);
    assert_eq!(
        &response[20..36],
        hex("00 09 00 04 00 00 04 14 00 0a 00 04 00 03 00 24")
    );
    let attributes: Vec<_> = (0..(MAX_DATAGRAM_BYTES - HEADER_BYTES) / ATTRIBUTE_HEADER_BYTES)
        .flat_map(|kind| {
            [
                u8::try_from(kind >> 8).unwrap(),
                u8::try_from(kind & 255).unwrap(),
                0,
                0,
            ]
        })
        .collect();
    assert!(
        binding_response(&request(&attributes), remote())
            .unwrap()
            .len()
            <= MAX_DATAGRAM_BYTES
    );
}

#[test]
fn malformed_headers_lengths_attributes_and_non_requests_are_silent_drops() {
    let valid = request(&hex("80 22 00 04 74 65 73 74"));
    for length in 0..valid.len() {
        assert!(
            binding_response(&valid[..length], remote()).is_err(),
            "prefix {length}"
        );
    }
    for (index, value) in [
        (0, 0x80),
        (0, 1),
        (1, 3),
        (1, 0x11),
        (4, 0),
        (2, 0xff),
        (3, 1),
    ] {
        let mut malformed = valid.clone();
        malformed[index] = value;
        assert!(binding_response(&malformed, remote()).is_err());
    }
    for attributes in [
        "80 22 ff ff",
        "80 22 00 01",
        "80 28 00 00",
        "80 28 00 08 00 00 00 00 00 00 00 00",
        "80 28 00 04 00 00 00 00",
        "80 28 00 04 00 00 00 00 80 22 00 00",
    ] {
        assert!(
            binding_response(&request(&hex(attributes)), remote()).is_err(),
            "{attributes}"
        );
    }
    let mut trailing = valid;
    trailing.push(0);
    assert_eq!(binding_response(&trailing, remote()), Err(Invalid::Header));
    assert_eq!(
        binding_response(&vec![0; MAX_DATAGRAM_BYTES + 1], remote()),
        Err(Invalid::Size)
    );
}
