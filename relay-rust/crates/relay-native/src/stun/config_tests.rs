use super::*;

#[test]
fn listener_configuration_is_bounded_and_unambiguous() {
    Config::default().validate().unwrap();
    Config {
        udp_addresses: Vec::new(),
        ..Config::default()
    }
    .validate()
    .unwrap();
    let address = "127.0.0.1:0".parse().unwrap();
    assert!(
        Config {
            udp_addresses: vec![address; 2],
            ..Config::default()
        }
        .validate()
        .is_err()
    );
    let addresses = (0..=MAX_LISTENERS)
        .map(|index| (Ipv4Addr::LOCALHOST, u16::try_from(index).unwrap()).into())
        .collect();
    assert!(
        Config {
            udp_addresses: addresses,
            ..Config::default()
        }
        .validate()
        .is_err()
    );
}
