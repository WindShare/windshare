use super::*;

#[cfg(feature = "hibernation")]
#[test]
fn stopping_restore_closes_surviving_sockets_and_commits_stopped() {
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
    let snapshots = snapshot_all(&relay);
    let last_session = relay.last_session();
    let intent = work(&fixture.stop(&mut relay));
    let record = next_record(&intent);
    for (surviving, connections) in [
        (&[key.sender(), key.receiver()][..], snapshots.as_slice()),
        (&[], &[]),
    ] {
        let (mut restored, step) = Relay::restore_hibernated(
            fixture.identity(2),
            Fixture::limits(),
            &Hibernated {
                record: Some(record),
                last_session,
                surviving,
                connections,
            },
            Trace::default(),
            fixture.at,
        )
        .unwrap();
        for &peer in surviving {
            assert!(
                step.outputs
                    .iter()
                    .any(|output| matches!(output, Output::Close(value) if *value == peer))
            );
        }
        let commit = work(&step);
        assert!(matches!(next_record(&commit), Record::Stopped(_)));
        let _ = complete(&mut restored, &commit, fixture.at);
        assert_eq!(restored.lifecycle.phase(), crate::lifecycle::Phase::Stopped);
        assert!(restored.established_connections().next().is_none());
    }
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
fn uncertain_registration_is_confirmed_before_a_queued_stop_selects_its_record() {
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
    assert!(matches!(uncertain.outputs.as_slice(), [Output::Retry(id)] if *id == publication.id));
    let retry = work(&relay.handle(Input::Retry(publication.id), fixture.at));
    assert_eq!(retry, publication);
    let completed = complete(&mut relay, &retry, fixture.at);
    assert!(!completed.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Registered(_),
            ..
        }
    )));
    let intent = work(&completed);
    assert!(
        matches!(&intent.operation, Storage::CompareExchange { expected: Some(expected), next: Some(next) }
        if **expected == next_record(&publication) && matches!(**next, Record::Stopping(_)))
    );
    let commit = work(&complete(&mut relay, &intent, fixture.at));
    assert!(matches!(next_record(&commit), Record::Stopped(_)));
    let stopped = complete(&mut relay, &commit, fixture.at);
    assert!(stopped.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Stopped(_),
            ..
        }
    )));
    assert!(
        complete(&mut relay, &publication, fixture.at)
            .outputs
            .is_empty()
    );
}

#[test]
fn uncertain_registration_keeps_owner_loss_and_grace_expiry_ordered() {
    use crate::authority::{self, StoredAuthority};

    let fixture = Fixture::new();
    let identity = fixture.identity(1);
    for committed in [false, true] {
        for disconnect in [false, true] {
            let mut relay = fixture.relay();
            let registration = fixture.begin_registration(&mut relay);
            let apply = |stored: Option<&StoredAuthority>, work: &Work| {
                authority::apply(identity.relay, identity.share, stored, &work.operation).unwrap()
            };
            let mut stored = if committed {
                apply(None, &registration)
            } else {
                None
            };
            let uncertain = relay.handle(
                Input::Completed {
                    id: registration.id,
                    result: WorkResult::Uncertain,
                },
                fixture.at,
            );
            assert!(
                matches!(uncertain.outputs.as_slice(), [Output::Retry(id)] if *id == registration.id)
            );

            let at = if disconnect {
                fixture.at
            } else {
                relay.deadline().unwrap()
            };
            let loss = relay.handle(
                if disconnect {
                    Input::Disconnected(connection(1))
                } else {
                    Input::Tick
                },
                at,
            );
            assert!(
                !loss
                    .outputs
                    .iter()
                    .any(|out| matches!(out, Output::Work(_)))
            );
            let grace_deadline = relay.lifecycle.deadline().unwrap();

            // Even if grace expires before storage recovers, deletion must follow
            // the exact registration and grace writes rather than race their retry.
            let elapsed = relay.handle(Input::Tick, grace_deadline);
            assert!(
                !elapsed
                    .outputs
                    .iter()
                    .any(|out| matches!(out, Output::Work(_)))
            );
            let retry = work(&relay.handle(Input::Retry(registration.id), grace_deadline));
            assert_eq!(retry, registration);
            stored = apply(stored.as_ref(), &retry);
            let confirmed = complete(&mut relay, &retry, grace_deadline);
            assert!(!confirmed.outputs.iter().any(|out| matches!(
                out,
                Output::Send {
                    control: Control::Registered(_),
                    ..
                }
            )));
            let grace = work(&confirmed);
            assert!(
                matches!(next_record(&grace), Record::Grace(route) if route.deadline() == grace_deadline)
            );
            stored = apply(stored.as_ref(), &grace);
            let cleanup = work(&complete(&mut relay, &grace, grace_deadline));
            assert!(matches!(
                &cleanup.operation,
                Storage::CompareExchange { next: None, .. }
            ));
            stored = apply(stored.as_ref(), &cleanup);
            let finished = complete(&mut relay, &cleanup, grace_deadline);
            assert!(stored.is_none());
            assert!(finished.outputs.is_empty());
            assert_eq!(relay.lifecycle.phase(), crate::lifecycle::Phase::Vacant);
            assert!(
                complete(&mut relay, &registration, grace_deadline)
                    .outputs
                    .is_empty()
            );
        }
    }
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
        None,
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
    let durable = fixture.begin_registration(&mut relay);
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
            id: durable.id,
            result: WorkResult::Complete,
        },
        fixture.at,
    );
    assert!(stale.outputs.is_empty());
    #[cfg(feature = "hibernation")]
    {
        let snapshots = snapshot_all(&relay);
        let (mut restored, _) = Relay::restore_hibernated(
            fixture.identity(2),
            Fixture::limits(),
            &Hibernated {
                record: Some(next_record(&durable)),
                last_session: relay.last_session(),
                surviving: &[key.sender(), key.receiver()],
                connections: &snapshots,
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
}
