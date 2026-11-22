use super::*;

fn with_policy(fixture: &Fixture, retirement: RetirementPolicy, business: usize) -> Relay {
    let limits = Limits {
        business_connections: business,
        retirement,
        ..Fixture::limits()
    };
    Relay::restore(
        fixture.identity(1),
        limits,
        None,
        Trace::default(),
        fixture.at,
    )
    .unwrap()
    .0
}

#[test]
fn physical_release_retains_detached_slot_until_closed_after_discard() {
    let fixture = Fixture::new();
    let mut relay = with_policy(&fixture, RetirementPolicy::PhysicalRelease, 2);
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    let receipt = receive(&mut relay, key, fixture.at);
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    let _ = relay.handle(Input::Disconnected(key.receiver()), fixture.at);
    assert!(relay.generations.is_detached(&key.receiver()));
    assert_eq!(relay.connection_usage().business, 2);
    assert_business_capacity_refused(&fixture, &mut relay, connection(3), fixture.at);

    for flow in flows {
        assert_eq!(
            relay.handle(Input::Discarded(flow), fixture.at).refusal,
            None
        );
    }
    // Physical release still holds the detached slot until Input::Closed confirms
    // transport buffers have drained.
    assert!(relay.generations.is_detached(&key.receiver()));
    assert_eq!(relay.connection_usage().business, 2);
    assert_business_capacity_refused(&fixture, &mut relay, connection(4), fixture.at);

    assert_eq!(
        relay
            .handle(Input::Closed(key.receiver()), fixture.at)
            .refusal,
        None
    );
    assert!(!relay.generations.contains(&key.receiver()));
    assert_eq!(relay.connection_usage().business, 1);
    let (replacement, _) = fixture.join(&mut relay, 5);
    assert_ne!(replacement.wire_id(), key.wire_id());
}

#[test]
fn after_discard_releases_detached_slot_immediately_while_sender_retains_debt() {
    let fixture = Fixture::new();
    let mut relay = with_policy(&fixture, RetirementPolicy::AfterDiscard, 2);
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    let receipt = receive(&mut relay, key, fixture.at);
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    let _ = relay.handle(Input::Disconnected(key.receiver()), fixture.at);
    // Unsubmitted queues still hold the slot until Input::Discarded arrives.
    assert!(relay.generations.is_detached(&key.receiver()));
    assert_business_capacity_refused(&fixture, &mut relay, connection(3), fixture.at);

    for flow in flows {
        assert_eq!(
            relay.handle(Input::Discarded(flow), fixture.at).refusal,
            None
        );
    }
    assert!(!relay.generations.contains(&key.receiver()));
    assert_eq!(relay.connection_usage().business, 1);
    // The healthy sender still retains submitted occupancy on its own ledger.
    assert_eq!(
        ledger_of(&relay, key.sender()).usage().submitted,
        Counts::new(1, 21)
    );
    let (replacement, _) = fixture.join(&mut relay, 4);
    assert_ne!(replacement.wire_id(), key.wire_id());
}
