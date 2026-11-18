use super::*;

#[test]
fn capacity_wire_matches_go_and_browser() {
    let frame = CapacityWait {
        session: RelaySessionId::new([1, 2, 3, 4, 5, 6, 7, 8]).unwrap(),
        direction: Direction::ToReceiver,
        milliseconds: 15_000,
    };
    assert_eq!(
        frame.encode().unwrap(),
        *b"WS2Z\x02\x01\0\0\x01\x02\x03\x04\x05\x06\x07\x08\0\0\x3a\x98"
    );
    assert!(
        CapacityWait {
            milliseconds: 60_001,
            ..frame
        }
        .encode()
        .is_err()
    );
    assert_eq!(
        &CapacityWait {
            milliseconds: 0,
            ..frame
        }
        .encode()
        .unwrap()[16..],
        &[0; 4]
    );
}
