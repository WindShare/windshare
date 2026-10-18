use super::*;

#[test]
fn hibernation_preserves_credit_receipts_identity_and_connection_deadlines() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    let receipt = receive(&mut relay, key, fixture.at);
    assert!(matches!(relay.checkpoint(), Err(Error::CheckpointBusy)));
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    activate(&mut relay, key, fixture.at);
    let encoded = relay.checkpoint().unwrap().encode();
    let checkpoint = Checkpoint::decode(&encoded, Fixture::limits()).unwrap();
    let (mut restored, turn) = Relay::restore(
        fixture.identity(2),
        Fixture::limits(),
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[key.sender(), key.receiver()],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert!(turn.outputs.is_empty());
    assert_eq!(
        turn.deadline,
        Some(
            fixture
                .at
                .saturating_add(crate::heartbeat::CONNECTION_SILENCE_TIMEOUT)
        )
    );
    let duplicate = restored.submit(&receipt, fixture.at, |_| panic!("old receipt resubmitted"));
    assert_eq!(
        duplicate.refusal,
        Some(Error::Flow(crate::flow::Refusal::SubmissionOrder))
    );
    assert_eq!(
        restored
            .handle(
                Input::Consumed {
                    connection: key.sender(),
                    session: flows[0].session().wire_id(),
                    total: Counts::new(1, 21)
                },
                fixture.at
            )
            .refusal,
        None
    );
    let (new_key, _) = fixture.join(&mut restored, 3);
    assert_ne!(new_key.wire_id(), key.wire_id());
    assert_eq!(
        restored.sessions[&key.wire_id()].session.phase(),
        crate::sessions::Phase::Active
    );
    assert_eq!(restored.sessions[&key.wire_id()].session.deadline(), None);
}

#[test]
fn retired_occupancy_survives_hibernation_and_keeps_original_cleanup_deadline() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    let receipt = receive(&mut relay, key, fixture.at);
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    let _ = relay.handle(
        Input::Cancel {
            participant: key.receiver(),
            session: key.wire_id(),
        },
        fixture.at,
    );
    for flow in flows {
        let _ = relay.handle(Input::Discarded(flow), fixture.at);
    }
    let deadline = relay.deadline().unwrap();
    let checkpoint =
        Checkpoint::decode(&relay.checkpoint().unwrap().encode(), Fixture::limits()).unwrap();
    let (mut restored, _) = Relay::restore(
        fixture.identity(2),
        Fixture::limits(),
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[key.sender(), key.receiver()],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        UnixMillis(fixture.at.0 + 100),
    )
    .unwrap();
    assert_eq!(restored.deadline(), Some(deadline));
    let closed = restored.handle(Input::Tick, deadline);
    assert!(
        closed
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == key.sender()))
    );
}

#[test]
fn snapshot_bounds_and_missing_delivery_state_fail_closed() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let bytes = relay.checkpoint().unwrap().encode();
    for truncated in [&bytes[..0], &bytes[..1], &bytes[..bytes.len() - 1]] {
        assert!(Checkpoint::decode(truncated, Fixture::limits()).is_err());
    }
    assert!(
        Relay::restore(
            fixture.identity(2),
            Fixture::limits(),
            &Restore {
                authority: Authority::Known(Some(stored)),
                surviving: &[connection(1)],
                responsibility: Responsibility::Unknown
            },
            Trace::default(),
            fixture.at
        )
        .is_err()
    );
    let checkpoint = Checkpoint::decode(&bytes, Fixture::limits()).unwrap();
    assert!(
        Relay::restore(
            fixture.identity(1),
            Fixture::limits(),
            &Restore {
                authority: Authority::Known(Some(stored)),
                surviving: &[connection(1)],
                responsibility: Responsibility::Checkpoint(&checkpoint)
            },
            Trace::default(),
            fixture.at
        )
        .is_err()
    );
}

#[test]
fn publication_deadline_includes_final_durable_confirmation() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let durable = fixture.begin_registration(&mut relay);
    let expired = UnixMillis(fixture.at.0 + REGISTRATION_BUDGET);
    let turn = complete(&mut relay, &durable, expired);
    assert!(
        turn.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == connection(1)))
    );
    assert!(!turn.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Registered(_),
            ..
        }
    )));
}
