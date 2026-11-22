use super::*;

#[test]
fn attachment_budget_validates_worst_case_snapshot_bounds() {
    assert!(AttachmentBudget::new(100, 100).is_none());
    assert!(AttachmentBudget::new(100, 200).is_none());
    let tight = AttachmentBudget::new(64, 16).unwrap();
    assert_eq!(tight.payload_bytes(), 48);
    assert_eq!(tight.check(1), Err(Error::CheckpointCapacity));

    let sufficient = AttachmentBudget::new(16_384, 512).unwrap();
    assert_eq!(sufficient.check(32), Ok(()));
    assert_eq!(sufficient.check(usize::MAX), Err(Error::CheckpointCapacity));
}

#[test]
fn snapshot_connection_rejects_pending_work_stop_barrier_and_unestablished_sockets() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let _ = relay.handle(Input::Connected(connection(2)), fixture.at);
    // Unestablished handshakes are not snapshotted.
    assert!(relay.snapshot_connection(connection(2)).unwrap().is_none());
    let pending = fixture.begin_registration(&mut relay);
    assert_eq!(
        relay.snapshot_connection(connection(1)).unwrap_err(),
        Error::CheckpointBusy
    );
    let _ = complete(&mut relay, &pending, fixture.at);
    assert!(relay.snapshot_connection(connection(1)).unwrap().is_some());

    // Pending STOP work refuses snapshotting.
    fixture.prepare_stop(&mut relay);
    let _intent = work(&fixture.stop(&mut relay));
    assert_eq!(
        relay.snapshot_connection(connection(1)).unwrap_err(),
        Error::CheckpointBusy
    );
}

#[test]
fn cold_wake_closes_unestablished_sockets_and_isolates_missing_receiver_snapshot() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let (lost, _) = fixture.join(&mut relay, 2);
    let (healthy, _) = fixture.join(&mut relay, 3);
    for key in [lost, healthy] {
        let receipt = receive(&mut relay, key, fixture.at);
        assert_eq!(
            relay
                .submit(&receipt, fixture.at, |_| Submission::Complete)
                .refusal,
            None
        );
    }
    activate(&mut relay, healthy, fixture.at);
    // Connection 4 is an unestablished handshake; connection 2 loses its snapshot.
    assert_eq!(
        relay
            .handle(Input::Connected(connection(4)), fixture.at)
            .refusal,
        None
    );
    let kept: Vec<_> = [connection(1), connection(3)]
        .into_iter()
        .map(|conn| {
            let encoded = relay.snapshot_connection(conn).unwrap().unwrap().encode();
            ConnectionSnapshot::decode(&encoded, relay.limits).unwrap()
        })
        .collect();
    let (mut restored, step) = Relay::restore_hibernated(
        fixture.identity(2),
        relay.limits,
        &Hibernated {
            record: Some(stored),
            last_session: relay.last_session(),
            surviving: &[connection(1), connection(2), connection(3), connection(4)],
            connections: &kept,
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    // Both the lost receiver and unestablished handshake are closed.
    for dropped in [connection(2), connection(4)] {
        assert!(
            step.outputs
                .iter()
                .any(|output| matches!(output, Output::Close(target) if *target == dropped))
        );
    }
    assert!(!step.outputs.iter().any(|output| matches!(
        output,
        Output::Close(target) if *target == connection(1) || *target == connection(3)
    )));
    // The orphaned flow on the sender's ledger is retired for discard, preserving both
    // the orphaned and healthy submitted charges.
    assert!(
        step.outputs
            .iter()
            .any(|output| matches!(output, Output::Discard(flow) if flow.session() == lost))
    );
    assert_eq!(
        ledger_of(&restored, connection(1)).usage().submitted,
        Counts::new(2, 42)
    );
    // Healthy sibling session continues forwarding unaffected.
    let forwarded = receive_from(&mut restored, healthy, healthy.sender(), fixture.at);
    assert_eq!(
        restored
            .submit(&forwarded, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
}

#[test]
fn cold_wake_preserves_last_session_watermark_and_rejects_foreign_or_duplicate_snapshots() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let (first, _) = fixture.join(&mut relay, 2);
    assert_eq!(relay.last_session(), 1);
    let sender_snap = ConnectionSnapshot::decode(
        &relay
            .snapshot_connection(connection(1))
            .unwrap()
            .unwrap()
            .encode(),
        relay.limits,
    )
    .unwrap();
    // Duplicate surviving connection is rejected.
    assert_eq!(
        Relay::restore_hibernated(
            fixture.identity(2),
            relay.limits,
            &Hibernated {
                record: Some(stored),
                last_session: 5,
                surviving: &[connection(1), connection(1)],
                connections: core::slice::from_ref(&sender_snap),
            },
            Trace::default(),
            fixture.at,
        )
        .unwrap_err(),
        Error::InvalidCheckpoint
    );
    // Snapshot not listed in surviving is rejected.
    assert_eq!(
        Relay::restore_hibernated(
            fixture.identity(2),
            relay.limits,
            &Hibernated {
                record: Some(stored),
                last_session: 5,
                surviving: &[],
                connections: core::slice::from_ref(&sender_snap),
            },
            Trace::default(),
            fixture.at,
        )
        .unwrap_err(),
        Error::InvalidCheckpoint
    );
    // Valid wake with higher persisted watermark never reissues earlier session IDs.
    let (mut restored, _) = Relay::restore_hibernated(
        fixture.identity(2),
        relay.limits,
        &Hibernated {
            record: Some(stored),
            last_session: 5,
            surviving: &[connection(1)],
            connections: &[sender_snap],
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert_eq!(restored.last_session(), 5);
    let (next, _) = fixture.join(&mut restored, 3);
    assert_ne!(next.wire_id(), first.wire_id());
    assert_eq!(restored.last_session(), 6);
}
