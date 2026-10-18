use super::*;

const RECEIVERS: usize = 8;
const LARGE_SESSION_LIMIT: usize = 128;

#[test]
fn handshake_metadata_and_checkpoint_size_do_not_scale_with_session_limit() {
    let fixture = Fixture::new();
    let mut checkpoint_bytes = None;
    for sessions in [
        1,
        LARGE_SESSION_LIMIT,
        LARGE_SESSION_LIMIT * LARGE_SESSION_LIMIT,
    ] {
        let mut limits = Fixture::limits();
        limits.sessions = sessions;
        limits.retained_flows_per_connection = sessions;
        let (mut relay, _) = Relay::restore(
            fixture.identity(1),
            limits,
            &Restore::restarted(None, Responsibility::Released { previous: None }),
            Trace::default(),
            fixture.at,
        )
        .unwrap();
        for id in 1..=RECEIVERS as u64 + 1 {
            assert_eq!(
                relay
                    .handle(Input::Connected(connection(id)), fixture.at)
                    .refusal,
                None
            );
            assert!(!relay.ledgers.contains_key(&connection(id)));
            assert!(relay.controls.contains_key(&connection(id)));
        }
        let encoded = relay.checkpoint().unwrap().encode();
        if let Some(expected) = &checkpoint_bytes {
            assert_eq!(&encoded, expected);
        } else {
            checkpoint_bytes = Some(encoded.clone());
        }
        Checkpoint::decode(&encoded, limits).unwrap();
    }
}

#[test]
fn sharing_allocates_sender_fanout_and_one_slot_per_receiver() {
    let fixture = Fixture::new();
    let mut limits = Fixture::limits();
    limits.sessions = LARGE_SESSION_LIMIT;
    limits.retained_flows_per_connection = LARGE_SESSION_LIMIT;
    let (mut relay, _) = Relay::restore(
        fixture.identity(1),
        limits,
        &Restore::restarted(None, Responsibility::Released { previous: None }),
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    fixture.register(&mut relay);
    assert_eq!(
        relay.ledgers[&connection(1)]
            .metadata_usage()
            .allocated_slots,
        0
    );
    for receiver in 2..=RECEIVERS as u64 + 1 {
        fixture.join(&mut relay, receiver);
        let usage = relay.ledgers[&connection(receiver)].metadata_usage();
        assert_eq!(usage.retained_flows, 1);
        assert_eq!(usage.allocated_slots, 1);
    }
    let usage = relay.ledgers[&connection(1)].metadata_usage();
    assert_eq!(usage.retained_flows, RECEIVERS);
    let allocated: usize = relay
        .ledgers
        .values()
        .map(|ledger| ledger.metadata_usage().allocated_slots)
        .sum();
    assert_eq!(allocated, RECEIVERS * 2);
}
