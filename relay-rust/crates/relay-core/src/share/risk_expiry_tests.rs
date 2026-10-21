use super::*;
use crate::share::responsibility::{GenerationState, ReservationKind};

const RETENTION: Duration = Duration::from_millis(200);
const LOSS_SPACING: Duration = Duration::from_millis(20);
const CHECKPOINT_BUDGET: CheckpointBudget = CheckpointBudget::new(16_384, 512).unwrap();

fn bounded(generations: usize, residual: Counts) -> RetirementPolicy {
    RetirementPolicy::Bounded {
        admission_grace: Duration::ZERO,
        risk_retention: RETENTION,
        generations,
        residual,
    }
}

#[test]
fn lost_feedback_saturates_each_limit_then_one_tick_restores_admission() {
    for retirement in [
        bounded(1, Counts::new(2, 42)),
        bounded(2, Counts::new(1, 42)),
        bounded(2, Counts::new(2, 21)),
    ] {
        let mut fixture = Fixture::new();
        let mut relay = relay(&fixture, 3, 2, retirement);
        fixture.register(&mut relay);
        let (healthy, healthy_flows) = fixture.join(&mut relay, 2);
        deliver(&fixture, &mut relay, healthy);
        consume(&mut relay, healthy, 1, fixture.at);
        // The blocked generation has the smaller ID: expiry must not depend on map order.
        let (first, first_flows) = fixture.join(&mut relay, 4);
        deliver(&fixture, &mut relay, first);
        let step = relay.handle(Input::Disconnected(first.receiver()), fixture.at);
        discard_outputs(&mut relay, step, fixture.at);
        let expires_at = fixture.at.saturating_add(RETENTION);
        fixture.at = fixture.at.saturating_add(LOSS_SPACING);
        let (blocked, _) = fixture.join(&mut relay, 3);
        deliver(&fixture, &mut relay, blocked);
        let step = relay.handle(Input::Disconnected(blocked.receiver()), fixture.at);
        discard_outputs(&mut relay, step, fixture.at);
        assert_eq!(relay.connection_usage().business, 3);
        assert_business_capacity_refused(&fixture, &mut relay, connection(5), fixture.at);
        download(&mut relay, healthy, healthy_flows[1], 2, fixture.at);
        let _ = relay.handle(Input::Tick, UnixMillis(expires_at.0 - 1));
        assert_eq!(relay.connection_usage().business, 3);

        let step = relay.handle(Input::Tick, expires_at);
        assert!(step.outputs.is_empty());
        assert_eq!(relay.connection_usage().business, 2);
        assert_eq!(relay.connection_usage().retired_generations, 1);
        assert_eq!(relay.connection_usage().residual, ONE_FRAME);
        assert_eq!(relay.delivery_usage(first.receiver()), None);
        assert_eq!(
            relay.deadline(),
            Some(expires_at.saturating_add(LOSS_SPACING))
        );
        assert!(relay.observer().0.iter().any(|event| matches!(
            event, Observation::Relay { connection: Some(id),
                decision: RelayDecision::RetirementRiskExpired { residual, expires_at: at }, .. }
                if *id == first.receiver() && *residual == ONE_FRAME && *at == expires_at
        )));

        fixture.at = expires_at;
        let replacement = ConnectionRef {
            generation: NonZeroU64::new(2).unwrap(),
            ..first.receiver()
        };
        let (next, _) = fixture.join_connection(&mut relay, replacement);
        deliver(&fixture, &mut relay, next);
        let before = relay.checkpoint().unwrap().encode();
        for connection in [first.receiver(), replacement] {
            for input in [
                Input::Consumed {
                    connection,
                    session: first.wire_id(),
                    total: ONE_FRAME,
                },
                Input::Settled {
                    connection,
                    session: first.wire_id(),
                    total: ONE_FRAME,
                },
                Input::Discarded(first_flows[1]),
            ] {
                assert_eq!(relay.handle(input, fixture.at).refusal, None);
            }
        }
        assert_eq!(relay.checkpoint().unwrap().encode(), before);
        download(&mut relay, healthy, healthy_flows[1], 3, fixture.at);
        let _ = relay.handle(Input::Tick, expires_at.saturating_add(LOSS_SPACING));
        assert_eq!(relay.connection_usage().retired_generations, 0);
        assert_eq!(relay.connection_usage().residual, Counts::ZERO);
    }
}

#[test]
fn expired_or_immediate_policy_still_waits_for_both_queue_receipts() {
    for risk_retention in [Duration::ZERO, RETENTION] {
        let fixture = Fixture::new();
        // No retired pool is available: expiry must also free a blocked business slot.
        let mut relay = relay(
            &fixture,
            2,
            1,
            RetirementPolicy::Bounded {
                admission_grace: Duration::ZERO,
                risk_retention,
                generations: 0,
                residual: Counts::ZERO,
            },
        );
        fixture.register(&mut relay);
        let (key, flows) = fixture.join(&mut relay, 2);
        deliver(&fixture, &mut relay, key);
        for flow in flows {
            assert_eq!(
                relay
                    .handle(
                        Input::Grant {
                            flow,
                            credit: ONE_FRAME
                        },
                        fixture.at
                    )
                    .refusal,
                None
            );
        }
        let to_sender = receive(&mut relay, key, fixture.at);
        let to_receiver = receive_from(&mut relay, key, key.sender(), fixture.at);
        let _ = relay.handle(Input::Disconnected(key.receiver()), fixture.at);
        let expiry = fixture.at.saturating_add(risk_retention);
        let _ = relay.handle(Input::Tick, expiry);
        assert_eq!(relay.connection_usage().business, 2);
        assert_eq!(
            relay.handle(Input::Discarded(flows[1]), expiry).refusal,
            None
        );
        assert_eq!(relay.connection_usage().business, 2);
        let before = relay.observer().0.len();
        assert_eq!(
            relay.handle(Input::Discarded(flows[0]), expiry).refusal,
            None
        );
        assert_eq!(relay.connection_usage().business, 1);
        assert_eq!(relay.delivery_usage(key.receiver()), None);
        assert!(!relay.observer().0[before..].iter().any(|event| matches!(
            event,
            Observation::Relay {
                decision: RelayDecision::Closed | RelayDecision::ResponsibilityReleased,
                ..
            } | Observation::Delivery(crate::flow::Observation {
                decision: crate::flow::Decision::Closed { .. },
                ..
            })
        )));
        for receipt in [to_sender, to_receiver] {
            assert!(
                relay
                    .submit(&receipt, expiry, |_| panic!("expired authority"))
                    .refusal
                    .is_some()
            );
        }
    }
}

#[test]
fn manifest_deadlines_survive_hibernation_and_lost_records_in_both_retirement_phases() {
    for retired in [false, true] {
        for missing in [false, true] {
            let fixture = Fixture::new();
            let limits = Fixture::limits();
            let mut relay = relay(
                &fixture,
                3,
                2,
                RetirementPolicy::Bounded {
                    admission_grace: LOSS_SPACING,
                    risk_retention: RETENTION,
                    generations: 1,
                    residual: limits.delivery.total(),
                },
            );
            let stored = fixture.register(&mut relay);
            let (healthy, healthy_flows) = fixture.join(&mut relay, 2);
            deliver(&fixture, &mut relay, healthy);
            consume(&mut relay, healthy, 1, fixture.at);
            let (lost, _) = fixture.join(&mut relay, 3);
            deliver(&fixture, &mut relay, lost);
            let step = relay.handle(Input::Disconnected(lost.receiver()), fixture.at);
            discard_outputs(&mut relay, step, fixture.at);
            let grace_end = fixture.at.saturating_add(LOSS_SPACING);
            let expiry = fixture.at.saturating_add(RETENTION);
            let start = if retired { grace_end } else { fixture.at };
            let _ = relay.handle(Input::Tick, start);
            let expected = relay
                .generations
                .retirement_state(&lost.receiver())
                .unwrap();
            for boot in 2..=4 {
                let now = UnixMillis(start.0 + u64::from(boot));
                let manifest = CheckpointManifest::decode(
                    &relay.checkpoint_manifest().unwrap().encode(),
                    relay.limits,
                )
                .unwrap();
                let records = relay
                    .checkpoint()
                    .unwrap()
                    .connections
                    .into_iter()
                    .filter(|record| !missing || record.connection() != lost.receiver())
                    .collect();
                let checkpoint =
                    Checkpoint::from_connections(&manifest, records, relay.limits).unwrap();
                let (mut restored, step) = Relay::restore(
                    fixture.identity(boot),
                    relay.limits,
                    &Restore {
                        authority: Authority::Known(Some(stored)),
                        surviving: &[healthy.sender(), healthy.receiver()],
                        responsibility: Responsibility::Checkpoint(&checkpoint),
                    },
                    Trace::default(),
                    now,
                )
                .unwrap();
                discard_outputs(&mut restored, step, now);
                let _ = restored.handle(Input::Disconnected(lost.receiver()), now);
                assert_eq!(
                    restored.generations.retirement_state(&lost.receiver()),
                    Some(expected)
                );
                assert_eq!(
                    restored.deadline(),
                    Some(if retired { expiry } else { grace_end })
                );
                relay = restored;
            }
            let checkpoint = relay.checkpoint().unwrap();
            let (mut restored, step) = Relay::restore(
                fixture.identity(5),
                relay.limits,
                &Restore {
                    authority: Authority::Known(Some(stored)),
                    surviving: &[healthy.sender(), healthy.receiver()],
                    responsibility: Responsibility::Checkpoint(&checkpoint),
                },
                Trace::default(),
                expiry,
            )
            .unwrap();
            discard_outputs(&mut restored, step, expiry);
            assert!(!restored.generations.contains(&lost.receiver()));
            assert_eq!(restored.connection_usage().business, 2);
            assert_eq!(restored.connection_usage().uncertain, Counts::ZERO);
            download(&mut restored, healthy, healthy_flows[1], 2, expiry);
        }
    }
}

#[test]
fn expiry_does_not_refund_delivery_to_a_communicating_destination() {
    let fixture = Fixture::new();
    let mut relay = relay(&fixture, 2, 1, bounded(1, ONE_FRAME));
    fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    let request = receive(&mut relay, key, fixture.at);
    assert_eq!(
        relay
            .submit(&request, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    activate(&mut relay, key, fixture.at);
    let data = receive_from(&mut relay, key, key.sender(), fixture.at);
    assert_eq!(
        relay
            .submit(&data, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    let step = relay.handle(Input::Disconnected(key.receiver()), fixture.at);
    discard_outputs(&mut relay, step, fixture.at);
    let sender_ledger = ledger_of(&relay, key.sender()).checkpoint().unwrap();
    relay
        .enable_checkpoints(CHECKPOINT_BUDGET, fixture.at)
        .unwrap();
    relay
        .flush_checkpoints(fixture.at, |_, _| Ok::<_, ()>(()))
        .unwrap();
    let _ = relay.handle(Input::Tick, fixture.at.saturating_add(RETENTION));
    assert_eq!(relay.delivery_usage(key.receiver()), None);
    assert_eq!(
        relay.delivery_usage(key.sender()).unwrap().submitted,
        ONE_FRAME
    );
    assert_eq!(
        ledger_of(&relay, key.sender()).checkpoint().unwrap(),
        sender_ledger
    );
    assert!(
        relay
            .sessions
            .routes()
            .contains_key(&(key.sender(), key.wire_id()))
    );
    assert!(
        !relay
            .sessions
            .routes()
            .contains_key(&(key.receiver(), key.wire_id()))
    );
    let mut targets = Vec::new();
    let limits = relay.limits;
    relay
        .flush_checkpoints(fixture.at.saturating_add(RETENTION), |target, bytes| {
            targets.push(target);
            let manifest = CheckpointManifest::decode(bytes, limits).unwrap();
            assert_eq!(manifest.generations().collect::<Vec<_>>(), [key.sender()]);
            Ok::<_, ()>(())
        })
        .unwrap();
    assert_eq!(targets, [CheckpointTarget::Manifest]);
}

#[test]
fn invalid_policy_and_inverted_persisted_deadlines_are_rejected() {
    let fixture = Fixture::new();
    let invalid = Limits {
        retirement: RetirementPolicy::Bounded {
            admission_grace: RETENTION,
            risk_retention: Duration::ZERO,
            generations: 1,
            residual: ONE_FRAME,
        },
        ..Fixture::limits()
    };
    assert!(matches!(
        Relay::restore(
            fixture.identity(1),
            invalid,
            &Restore::restarted(None, Responsibility::Released { previous: None }),
            Trace::default(),
            fixture.at
        ),
        Err(Error::Configuration)
    ));

    let mut relay = relay(&fixture, 2, 1, bounded(1, ONE_FRAME));
    fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    deliver(&fixture, &mut relay, key);
    let _ = relay.handle(Input::Disconnected(key.receiver()), fixture.at);
    let mut manifest = relay.checkpoint_manifest().unwrap();
    let reservation = manifest
        .reservations
        .iter_mut()
        .find(|r| r.connection == key.receiver())
        .unwrap();
    let ReservationKind::Business { state, .. } = &mut reservation.kind else {
        panic!("business generation");
    };
    *state = GenerationState::Detached(Detached::Quiescing {
        retire_at: fixture.at.saturating_add(RETENTION),
        risk_expires_at: fixture.at,
    });
    assert_eq!(
        CheckpointManifest::decode(&manifest.encode(), relay.limits),
        Err(Error::InvalidCheckpoint)
    );
}
