use super::*;

#[test]
fn stopped_restore_waits_for_every_surviving_receiver_before_commit() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.prepare_stop(&mut relay);
    fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    let receipt = receive(&mut relay, key, fixture.at);
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    let checkpoint = Checkpoint::decode(
        &relay.checkpoint().unwrap().encode(),
        Fixture::limits(),
        SLOTS,
    )
    .unwrap();
    let intent = work(&fixture.stop(&mut relay));
    let record = next_record(&intent);
    let (mut restored, turn) = Relay::restore(
        fixture.identity(2),
        Fixture::limits(),
        &Restore {
            record: Some(record),
            surviving: &[key.sender(), key.receiver()],
            checkpoint: Some(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert!(
        turn.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == key.receiver()))
    );
    let sender_closed = restored.handle(Input::Closed(key.sender()), fixture.at);
    assert!(
        !sender_closed
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Work(_)))
    );
    let receiver_closed = restored.handle(Input::Closed(key.receiver()), fixture.at);
    assert!(matches!(
        next_record(&work(&receiver_closed)),
        Record::Stopped(_)
    ));
}

#[test]
fn stop_does_not_request_physical_cleanup_before_durable_intent_even_on_timeout() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.prepare_stop(&mut relay);
    fixture.register(&mut relay);
    fixture.join(&mut relay, 2);
    let intent = work(&fixture.stop(&mut relay));
    let late = UnixMillis(fixture.at.0 + SETTLEMENT_BUDGET);
    let timed_out = relay.handle(Input::Tick, late);
    assert!(
        !timed_out
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(_) | Output::Discard(_)))
    );
    let cleanup = complete(&mut relay, &intent, late);
    assert!(
        cleanup
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(_)))
    );
}

#[test]
fn uncertain_publication_is_reconciled_before_a_queued_stop_selects_its_record() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.prepare_stop(&mut relay);
    let publication = fixture.begin_registration(&mut relay);
    assert!(
        !fixture
            .stop(&mut relay)
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Work(_)))
    );
    let uncertain = relay.handle(
        Input::Completed {
            id: publication.id,
            result: WorkResult::Uncertain,
        },
        fixture.at,
    );
    let reconcile = work(&uncertain);
    assert!(matches!(reconcile.operation, Storage::RemovePublication(_)));
    let intent = work(&complete(&mut relay, &reconcile, fixture.at));
    assert!(
        matches!(intent.operation, Storage::CompareExchange { expected: None, next: Some(ref record) } if matches!(**record, Record::Stopping(_)))
    );
    let cleanup = complete(&mut relay, &intent, fixture.at);
    let quiesce = work(&cleanup);
    let _ = relay.handle(Input::Closed(connection(1)), fixture.at);
    let commit = work(&complete(&mut relay, &quiesce, fixture.at));
    assert!(matches!(next_record(&commit), Record::Stopped(_)));
}

#[test]
fn startup_exhaustion_does_not_leak_credit_and_reclaimed_capacity_admits_again() {
    let fixture = Fixture::new();
    let mut limits = Fixture::limits();
    let startup = Counts::new(1, u64::from(OpaqueHeader::MAX_ENCODED_BYTES));
    limits.delivery = crate::flow::Limits::new(
        startup,
        startup,
        startup,
        Duration::from_millis(SETTLEMENT_BUDGET),
    )
    .unwrap();
    let (mut relay, _) = Relay::restore(
        fixture.identity(1),
        limits,
        &Restore::restarted(None),
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    let _ = relay.handle(Input::Connected(connection(3)), fixture.at);
    let denied = fixture.control(&mut relay, connection(3), "joinB64", fixture.at);
    assert!(!denied.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Grant(_),
            ..
        }
    )));
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
    let (next, _) = fixture.join(&mut relay, 4);
    assert_ne!(next.wire_id(), key.wire_id());
}

#[test]
fn stale_completion_and_generation_feedback_cannot_release_new_occupancy() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let publication = fixture.begin_registration(&mut relay);
    let durable = work(&complete(&mut relay, &publication, fixture.at));
    let _ = complete(&mut relay, &durable, fixture.at);
    let (key, _) = fixture.join(&mut relay, 2);
    let receipt = receive(&mut relay, key, fixture.at);
    let _ = relay.submit(&receipt, fixture.at, |_| Submission::Complete);
    let wrong = ConnectionRef {
        generation: NonZeroU64::new(2).unwrap(),
        ..key.sender()
    };
    let feedback = relay.handle(
        Input::Consumed {
            connection: wrong,
            session: key.wire_id(),
            total: Counts::new(1, 21),
        },
        fixture.at,
    );
    assert!(feedback.outputs.is_empty());
    let stale = relay.handle(
        Input::Completed {
            id: publication.id,
            result: WorkResult::Complete,
        },
        fixture.at,
    );
    assert!(stale.outputs.is_empty());
    let snapshot = relay.checkpoint().unwrap();
    let (mut restored, _) = Relay::restore(
        fixture.identity(2),
        Fixture::limits(),
        &Restore {
            record: Some(next_record(&durable)),
            surviving: &[key.sender(), key.receiver()],
            checkpoint: Some(&snapshot),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert_eq!(
        restored
            .submit(&receipt, fixture.at, |_| panic!("duplicate handoff"))
            .refusal,
        Some(Error::Flow(crate::flow::Refusal::SubmissionOrder))
    );
}
