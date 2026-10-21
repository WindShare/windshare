use super::*;
use std::vec;

const ONE: Counts = Counts::new(1, 21);
const LOSS_GRACE_MS: u64 = 100;
const LOSS_RISK_RETENTION: Duration = Duration::from_secs(10);

#[test]
fn unknown_authority_or_invalid_reservations_refuse_even_with_healthy_records() {
    let fixture = Fixture::new();
    let mut original = fixture.relay();
    fixture.register(&mut original);
    let checkpoint = original.checkpoint().unwrap();
    assert!(matches!(
        Relay::restore(
            fixture.identity(2),
            original.limits,
            &Restore {
                authority: Authority::Unknown,
                surviving: &[connection(1)],
                responsibility: Responsibility::Checkpoint(&checkpoint)
            },
            Trace::default(),
            fixture.at,
        ),
        Err(Error::MissingAuthority)
    ));
    let mut manifest = original.checkpoint_manifest().unwrap();
    let crate::share::responsibility::ReservationKind::Business { bound, .. } =
        &mut manifest.reservations[0].kind
    else {
        panic!("business reservation")
    };
    *bound = Counts::ZERO;
    assert!(matches!(
        Checkpoint::from_connections(&manifest, Vec::new(), original.limits),
        Err(Error::InvalidCheckpoint)
    ));
}

fn relay(fixture: &Fixture, grace: u64, residual: Counts) -> Relay {
    let limits = Limits {
        business_connections: 3,
        retirement: RetirementPolicy::Bounded {
            admission_grace: Duration::from_millis(grace),
            risk_retention: LOSS_RISK_RETENTION,
            generations: 2,
            residual,
        },
        ..Fixture::limits()
    };
    Relay::restore(
        fixture.identity(1),
        limits,
        &Restore::restarted(None, Responsibility::Released { previous: None }),
        Trace::default(),
        fixture.at,
    )
    .unwrap()
    .0
}

fn active(
    fixture: &Fixture,
    relay: &mut Relay,
    receiver: u64,
) -> (SessionKey, [FlowKey; 2], ReceivedFrame) {
    let (key, flows) = fixture.join(relay, receiver);
    let receipt = receive(relay, key, fixture.at);
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    activate(relay, key, fixture.at);
    let forward = receive_from(relay, key, key.sender(), fixture.at);
    assert_eq!(
        relay
            .submit(&forward, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    (key, flows, forward)
}

fn discard(relay: &mut Relay, step: RelayStep, at: UnixMillis) {
    for output in step.outputs {
        if let Output::Discard(flow) = output {
            assert_eq!(relay.handle(Input::Discarded(flow), at).refusal, None);
        }
    }
}

fn without(relay: &Relay, connection: ConnectionRef) -> Checkpoint {
    let records = relay
        .generations
        .ledger_ids()
        .filter(|value| *value != connection)
        .map(|value| relay.checkpoint_connection(value).unwrap())
        .collect();
    Checkpoint::from_connections(&relay.checkpoint_manifest().unwrap(), records, relay.limits)
        .unwrap()
}

#[test]
fn lost_receiver_preserves_sibling_delivery_deadline_and_sender_reverse_debt() {
    let fixture = Fixture::new();
    let bound = Fixture::limits().delivery.total();
    let mut original = relay(&fixture, LOSS_GRACE_MS, bound);
    let stored = fixture.register(&mut original);
    let (lost, lost_flows, receipt) = active(&fixture, &mut original, 2);
    let (healthy, healthy_flows) = fixture.join(&mut original, 3);
    let healthy_before = ledger_of(&original, healthy.receiver())
        .checkpoint()
        .unwrap();
    let sender_before = ledger_of(&original, healthy.sender())
        .snapshot(healthy_flows[0])
        .unwrap();
    let deadline = original.sessions.entries()[&healthy.wire_id()]
        .session
        .deadline();
    let checkpoint = without(&original, lost.receiver());
    let (mut recovered, step) = Relay::restore(
        fixture.identity(2),
        original.limits,
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[lost.sender(), lost.receiver(), healthy.receiver()],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert_eq!(
        step.outputs
            .iter()
            .filter_map(|output| match output {
                Output::Close(connection) => Some(*connection),
                _ => None,
            })
            .collect::<Vec<_>>(),
        vec![lost.receiver()]
    );
    assert!(!recovered.sessions.entries().contains_key(&lost.wire_id()));
    assert_eq!(
        recovered.sessions.entries()[&healthy.wire_id()]
            .session
            .deadline(),
        deadline
    );
    assert_eq!(
        ledger_of(&recovered, healthy.receiver())
            .checkpoint()
            .unwrap(),
        healthy_before
    );
    assert_eq!(
        ledger_of(&recovered, healthy.sender())
            .snapshot(healthy_flows[0])
            .unwrap(),
        sender_before
    );
    assert_eq!(
        ledger_of(&recovered, lost.sender())
            .snapshot(lost_flows[0])
            .unwrap()
            .submitted,
        ONE
    );
    assert_eq!(
        recovered.unavailable_reservation(lost.receiver()),
        Some(bound)
    );
    assert!(recovered.delivery_usage(lost.receiver()).is_none());
    assert!(
        recovered
            .submit(&receipt, fixture.at, |_| panic!("revoked submission"))
            .refusal
            .is_some()
    );
    assert_eq!(
        recovered
            .handle(Input::Connected(lost.receiver()), fixture.at)
            .refusal,
        Some(Error::UnknownConnection)
    );
    discard(&mut recovered, step, fixture.at);

    let bootstrap = receive(&mut recovered, healthy, fixture.at);
    assert_eq!(
        recovered
            .submit(&bootstrap, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    activate(&mut recovered, healthy, fixture.at);
    let data = receive_from(&mut recovered, healthy, healthy.sender(), fixture.at);
    assert_eq!(
        recovered
            .submit(&data, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );

    reconnect_after_recovery(&fixture, recovered, stored, lost, healthy);
}

fn reconnect_after_recovery(
    fixture: &Fixture,
    mut recovered: Relay,
    stored: Record,
    lost: SessionKey,
    healthy: SessionKey,
) {
    let bound = recovered.limits.delivery.total();
    let grace_end = UnixMillis(fixture.at.0 + LOSS_GRACE_MS);
    for incarnation in 3..=4 {
        let checkpoint =
            Checkpoint::decode(&recovered.checkpoint().unwrap().encode(), recovered.limits)
                .unwrap();
        let (next, step) = Relay::restore(
            fixture.identity(incarnation),
            recovered.limits,
            &Restore {
                authority: Authority::Known(Some(stored)),
                surviving: &[healthy.sender(), healthy.receiver()],
                responsibility: Responsibility::Checkpoint(&checkpoint),
            },
            Trace::default(),
            UnixMillis(fixture.at.0 + u64::from(incarnation)),
        )
        .unwrap();
        recovered = next;
        discard(&mut recovered, step, fixture.at);
        assert_eq!(
            recovered
                .generations
                .retirement_state(&lost.receiver())
                .unwrap(),
            Detached::Quiescing {
                retire_at: grace_end,
                risk_expires_at: fixture.at.saturating_add(LOSS_RISK_RETENTION),
            }
        );
        assert_eq!(
            recovered.unavailable_reservation(lost.receiver()),
            Some(bound)
        );
    }
    let _ = recovered.handle(Input::Tick, grace_end);
    assert_eq!(recovered.connection_usage().residual, bound);
    assert_eq!(recovered.connection_usage().uncertain, bound);
    let replacement = ConnectionRef {
        generation: NonZeroU64::new(2).unwrap(),
        ..lost.receiver()
    };
    let (next, _) = fixture.join_connection(&mut recovered, replacement);
    assert_ne!(next.wire_id(), lost.wire_id());
    assert_ne!(next.wire_id(), healthy.wire_id());
    let before = ledger_of(&recovered, replacement).checkpoint().unwrap();
    for connection in [lost.receiver(), replacement] {
        let _ = recovered.handle(
            Input::Settled {
                connection,
                session: lost.wire_id(),
                total: ONE,
            },
            grace_end,
        );
    }
    assert_eq!(
        ledger_of(&recovered, replacement).checkpoint().unwrap(),
        before
    );
    assert_eq!(
        recovered.unavailable_reservation(lost.receiver()),
        Some(bound)
    );
    assert!(recovered.observer().0.iter().any(|entry| matches!(entry,
        Observation::Relay { connection: Some(connection), decision: RelayDecision::RecoveryIsolated { reserved }, .. }
        if *connection == lost.receiver() && *reserved == bound)));
}

#[test]
fn missing_record_risk_exhaustion_blocks_replacement_without_stopping_healthy_flow() {
    let fixture = Fixture::new();
    let mut original = relay(&fixture, 0, ONE);
    let stored = fixture.register(&mut original);
    let (lost, _, _) = active(&fixture, &mut original, 2);
    let (healthy, flows, _) = active(&fixture, &mut original, 3);
    let checkpoint = without(&original, lost.receiver());
    let (mut recovered, step) = Relay::restore(
        fixture.identity(2),
        original.limits,
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[healthy.sender(), healthy.receiver()],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    discard(&mut recovered, step, fixture.at);
    assert_eq!(recovered.connection_usage().business, 3);
    assert_business_capacity_refused(&fixture, &mut recovered, connection(4), fixture.at);
    assert_eq!(
        recovered
            .handle(
                Input::Grant {
                    flow: flows[1],
                    credit: ONE
                },
                fixture.at
            )
            .refusal,
        None
    );
    let receipt = receive_from(&mut recovered, healthy, healthy.sender(), fixture.at);
    assert_eq!(
        recovered
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    let _ = recovered.handle(Input::Closed(lost.receiver()), fixture.at);
    assert_eq!(recovered.unavailable_reservation(lost.receiver()), None);
    assert_eq!(
        recovered
            .handle(Input::Connected(connection(4)), fixture.at)
            .refusal,
        None
    );
}

#[test]
fn corrupt_or_duplicate_receiver_records_are_isolated_without_rejecting_the_sender() {
    let fixture = Fixture::new();
    let mut original = fixture.relay();
    let stored = fixture.register(&mut original);
    let (lost, _, _) = active(&fixture, &mut original, 2);
    let (healthy, _, _) = active(&fixture, &mut original, 3);
    let manifest = original.checkpoint_manifest().unwrap();
    let sender = original.checkpoint_connection(lost.sender()).unwrap();
    let receiver = original.checkpoint_connection(lost.receiver()).unwrap();
    let sibling = original.checkpoint_connection(healthy.receiver()).unwrap();
    let mut corrupt = receiver.clone();
    corrupt.identity = fixture.identity(9);
    for records in [
        vec![sender.clone(), corrupt, sibling.clone()],
        vec![sender, receiver.clone(), receiver, sibling],
    ] {
        let checkpoint = Checkpoint::from_connections(&manifest, records, original.limits).unwrap();
        let (recovered, _) = Relay::restore(
            fixture.identity(2),
            original.limits,
            &Restore {
                authority: Authority::Known(Some(stored)),
                surviving: &[lost.sender(), lost.receiver(), healthy.receiver()],
                responsibility: Responsibility::Checkpoint(&checkpoint),
            },
            Trace::default(),
            fixture.at,
        )
        .unwrap();
        assert!(
            recovered
                .sessions
                .entries()
                .contains_key(&healthy.wire_id())
        );
        assert!(!recovered.sessions.entries().contains_key(&lost.wire_id()));
        assert!(recovered.unavailable_reservation(lost.receiver()).is_some());
    }
}

#[test]
fn losing_sender_delivery_state_retires_all_its_sessions_and_persists_owner_grace() {
    let fixture = Fixture::new();
    let mut original = fixture.relay();
    let stored = fixture.register(&mut original);
    let (first, _, _) = active(&fixture, &mut original, 2);
    let (second, _, _) = active(&fixture, &mut original, 3);
    let checkpoint = without(&original, first.sender());
    let (recovered, step) = Relay::restore(
        fixture.identity(2),
        original.limits,
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[first.sender(), first.receiver(), second.receiver()],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert!(recovered.sessions.entries().is_empty());
    for connection in [first.sender(), first.receiver(), second.receiver()] {
        assert!(
            step.outputs
                .iter()
                .any(|output| matches!(output, Output::Close(value) if *value == connection))
        );
    }
    assert!(matches!(next_record(&work(&step)), Record::Grace(_)));
    assert_eq!(
        recovered
            .delivery_usage(first.receiver())
            .unwrap()
            .submitted,
        ONE
    );
    assert_eq!(
        recovered
            .delivery_usage(second.receiver())
            .unwrap()
            .submitted,
        ONE
    );
}

#[test]
fn losing_a_retired_record_uses_its_persisted_reservation_and_cannot_resurrect_it() {
    let fixture = Fixture::new();
    let mut original = relay(&fixture, 0, ONE);
    let stored = fixture.register(&mut original);
    let (lost, _, _) = active(&fixture, &mut original, 2);
    let step = original.handle(Input::Disconnected(lost.receiver()), fixture.at);
    discard(&mut original, step, fixture.at);
    assert_eq!(original.connection_usage().residual, ONE);
    let checkpoint = without(&original, lost.receiver());
    let (mut recovered, step) = Relay::restore(
        fixture.identity(2),
        original.limits,
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[lost.sender()],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    discard(&mut recovered, step, fixture.at);
    assert_eq!(
        recovered.unavailable_reservation(lost.receiver()),
        Some(ONE)
    );
    let mut stale = original.checkpoint_connection(lost.receiver()).unwrap();
    stale.identity = fixture.identity(2);
    let manifest = recovered.checkpoint_manifest().unwrap();
    let checkpoint = Checkpoint::from_connections(
        &manifest,
        vec![
            recovered.checkpoint_connection(lost.sender()).unwrap(),
            stale,
        ],
        recovered.limits,
    )
    .unwrap();
    assert_eq!(checkpoint.connections.len(), 1);
    assert_eq!(manifest, checkpoint.manifest());
}

#[test]
fn duplicate_connection_events_preserve_exact_and_unavailable_responsibility() {
    let fixture = Fixture::new();
    for missing_record in [false, true] {
        let mut original = relay(&fixture, LOSS_GRACE_MS, Fixture::limits().delivery.total());
        let stored = fixture.register(&mut original);
        let (lost, _, _) = active(&fixture, &mut original, 2);
        let checkpoint = if missing_record {
            without(&original, lost.receiver())
        } else {
            original.checkpoint().unwrap()
        };
        let (mut recovered, step) = Relay::restore(
            fixture.identity(2),
            original.limits,
            &Restore {
                authority: Authority::Known(Some(stored)),
                surviving: &[lost.sender()],
                responsibility: Responsibility::Checkpoint(&checkpoint),
            },
            Trace::default(),
            fixture.at,
        )
        .unwrap();
        discard(&mut recovered, step, fixture.at);
        let before = recovered.checkpoint().unwrap().encode();
        let usage = recovered.connection_usage();
        assert_eq!(
            recovered.delivery_usage(lost.receiver()).is_none(),
            missing_record
        );
        assert_eq!(
            recovered.unavailable_reservation(lost.receiver()).is_some(),
            missing_record
        );

        let refused = recovered.handle(Input::Connected(lost.receiver()), fixture.at);
        assert_eq!(refused.refusal, Some(Error::UnknownConnection));
        assert!(refused.outputs.iter().any(|output| matches!(
            output, Output::Close(connection) if *connection == lost.receiver()
        )));
        let _ = recovered.handle(Input::Disconnected(lost.receiver()), fixture.at);
        assert_eq!(recovered.connection_usage(), usage);
        assert_eq!(recovered.checkpoint().unwrap().encode(), before);

        let _ = recovered.handle(Input::Closed(lost.receiver()), fixture.at);
        assert_eq!(recovered.delivery_usage(lost.receiver()), None);
        assert_eq!(recovered.unavailable_reservation(lost.receiver()), None);
        let released = recovered.checkpoint().unwrap().encode();
        let _ = recovered.handle(Input::Closed(lost.receiver()), fixture.at);
        assert_eq!(recovered.checkpoint().unwrap().encode(), released);
    }
}
