use super::*;

const PROXY: IpAddr = IpAddr::V4(std::net::Ipv4Addr::LOCALHOST);
const CLIENT: IpAddr = IpAddr::V4(std::net::Ipv4Addr::new(192, 0, 2, 1));
const SOURCE_HEADER: &str = "cf-connecting-ip";

#[test]
fn direct_and_untrusted_peers_ignore_forwarded_identity() {
    let mut headers = HeaderMap::new();
    for name in [SOURCE_HEADER, "x-forwarded-for", "x-real-ip", "forwarded"] {
        headers.insert(name, "198.51.100.1".parse().unwrap());
    }
    for policy in [
        SourcePolicy::default(),
        SourcePolicy::trusted_proxy([PROXY], SOURCE_HEADER).unwrap(),
    ] {
        assert_eq!(policy.resolve(CLIENT, &headers).unwrap(), CLIENT);
        assert_eq!(
            policy
                .resolve("::ffff:192.0.2.1".parse().unwrap(), &headers)
                .unwrap(),
            CLIENT
        );
    }
    assert_eq!(
        SourcePolicy::default().resolve(PROXY, &headers).unwrap(),
        PROXY
    );
}

#[test]
fn trusted_proxy_requires_one_unambiguous_ip_and_normalizes_mapped_ipv4() {
    let policy = SourcePolicy::trusted_proxy([PROXY], SOURCE_HEADER).unwrap();
    let mut headers = HeaderMap::new();
    assert!(policy.resolve(PROXY, &headers).is_err());
    for value in [
        "",
        "unknown",
        "192.0.2.1:80",
        "[2001:db8::1]",
        "fe80::1%eth0",
        "192.0.2.1, 198.51.100.1",
    ] {
        headers.insert(SOURCE_HEADER, value.parse().unwrap());
        assert!(policy.resolve(PROXY, &headers).is_err(), "{value}");
    }
    for value in ["192.0.2.1", "::ffff:192.0.2.1", "2001:db8::1"] {
        headers.insert(SOURCE_HEADER, value.parse().unwrap());
        let expected = value.parse::<IpAddr>().unwrap().to_canonical();
        assert_eq!(
            policy
                .resolve("::ffff:127.0.0.1".parse().unwrap(), &headers)
                .unwrap(),
            expected
        );
    }
    headers.append(SOURCE_HEADER, "2001:db8::1".parse().unwrap());
    assert!(policy.resolve(PROXY, &headers).is_err());
}

#[test]
fn trust_configuration_requires_explicit_peers_and_a_valid_header() {
    assert!(SourcePolicy::trusted_proxy([], SOURCE_HEADER).is_err());
    for header in ["", "invalid header", "x-source\r\n"] {
        assert!(SourcePolicy::trusted_proxy([PROXY], header).is_err());
    }
    let policy =
        SourcePolicy::trusted_proxy(["::ffff:127.0.0.1".parse().unwrap()], "X-Real-IP").unwrap();
    let mut headers = HeaderMap::new();
    headers.insert("x-real-ip", "192.0.2.1".parse().unwrap());
    assert_eq!(policy.resolve(PROXY, &headers).unwrap(), CLIENT);
}
