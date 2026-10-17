use super::*;
use crate::flow::Usage;

const ONE_FRAME: Counts = Counts::new(1, 21);

#[path = "retirement_tests.rs"]
mod retirement;

fn bounded_relay(fixture: &Fixture, generations: usize) -> Relay {
    let mut limits = Fixture::limits();
    limits.connections = generations;
    Relay::restore(
        fixture.identity(1),
        limits,
        &Restore::restarted(None, Responsibility::Released),
        Trace::default(),
        fixture.at,
    )
    .unwrap()
    .0
}

fn deliver(fixture: &Fixture, relay: &mut Relay, key: SessionKey) {
    let request = receive(relay, key, fixture.at);
    assert_eq!(
        relay
            .submit(&request, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    activate(relay, key, fixture.at);
    assert_eq!(
        relay
            .handle(
                Input::Consumed {
                    connection: key.sender(),
                    session: key.wire_id(),
                    total: ONE_FRAME,
                },
                fixture.at
            )
            .refusal,
        None
    );
    let data = receive_from(relay, key, key.sender(), fixture.at);
    assert_eq!(
        relay
            .submit(&data, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
}

fn discard_outputs(relay: &mut Relay, turn: Turn, at: UnixMillis) {
    for output in turn.outputs {
        if let Output::Discard(flow) = output {
            assert_eq!(relay.handle(Input::Discarded(flow), at).refusal, None);
        }
    }
}

#[test]
fn disconnect_revokes_grants_but_requires_separate_queue_and_transport_evidence() {
    let fixture = Fixture::new();
    let mut relay = bounded_relay(&fixture, 2);
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    deliver(&fixture, &mut relay, key);
    assert_eq!(
        relay
            .handle(
                Input::Grant {
                    flow: flows[1],
                    credit: Counts::new(2, 42)
                },
                fixture.at
            )
            .refusal,
        None
    );
    let queued = receive_from(&mut relay, key, key.sender(), fixture.at);
    let detached = relay.handle(Input::Disconnected(key.receiver()), fixture.at);
    assert!(!relay.connections.contains_key(&key.receiver()));
    assert_eq!(
        relay.delivery_usage(key.receiver()),
        Some(relay.ledgers[&key.receiver()].usage())
    );
    assert_eq!(
        relay.ledgers[&key.receiver()].usage(),
        Usage {
            granted: Counts::ZERO,
            received: ONE_FRAME,
            submitted: ONE_FRAME,
        }
    );
    assert!(
        relay
            .submit(&queued, fixture.at, |_| panic!("revoked handoff"))
            .refusal
            .is_some()
    );
    assert!(matches!(relay.checkpoint(), Err(Error::CheckpointBusy)));
    discard_outputs(&mut relay, detached, fixture.at);
    assert_eq!(
        relay.ledgers[&key.receiver()].usage(),
        Usage {
            submitted: ONE_FRAME,
            ..Usage::default()
        }
    );
    assert_eq!(
        relay
            .handle(Input::Connected(connection(3)), fixture.at)
            .refusal,
        Some(Error::Capacity)
    );
    assert!(relay.checkpoint().is_ok());
    let _ = relay.handle(Input::Closed(key.receiver()), fixture.at);
    assert_eq!(relay.delivery_usage(key.receiver()), None);
    assert_eq!(
        relay
            .handle(Input::Connected(connection(3)), fixture.at)
            .refusal,
        None
    );
}

#[test]
fn physical_release_policy_keeps_unreleased_admission_across_recovery() {
    let fixture = Fixture::new();
    let mut relay = bounded_relay(&fixture, 2);
    let stored = fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    deliver(&fixture, &mut relay, key);
    let mut deadline = None;
    for incarnation in 2..=4 {
        let now = UnixMillis(fixture.at.0 + u64::from(incarnation));
        let checkpoint =
            Checkpoint::decode(&relay.checkpoint().unwrap().encode(), relay.limits).unwrap();
        let (mut restored, turn) = Relay::restore(
            fixture.identity(incarnation),
            relay.limits,
            &Restore {
                authority: Authority::Known(Some(stored)),
                surviving: &[key.sender()],
                responsibility: Responsibility::Checkpoint(&checkpoint),
            },
            Trace::default(),
            now,
        )
        .unwrap();
        assert!(!turn.outputs.iter().any(|output| matches!(
            output,
            Output::Send {
                control: Control::Grant(_) | Control::Descriptor { .. },
                ..
            }
        )));
        discard_outputs(&mut restored, turn, now);
        assert_eq!(
            restored.ledgers[&key.receiver()].usage().submitted,
            ONE_FRAME
        );
        assert!(!restored.connections.contains_key(&key.receiver()));
        if let Some(original) = deadline {
            assert_eq!(restored.deadline(), Some(original));
        } else {
            deadline = restored.deadline();
        }
        assert_eq!(
            restored
                .handle(Input::Connected(connection(3)), now)
                .refusal,
            Some(Error::Capacity)
        );
        relay = restored;
    }
    let deadline = deadline.unwrap();
    let _ = relay.handle(Input::Tick, deadline);
    assert_eq!(relay.ledgers[&key.receiver()].usage().submitted, ONE_FRAME);
    assert_eq!(
        relay
            .handle(Input::Connected(connection(3)), deadline)
            .refusal,
        Some(Error::Capacity)
    );
    assert!(relay.deadline().is_none());
}

#[test]
fn restart_with_no_sockets_still_recovers_transport_debt() {
    let fixture = Fixture::new();
    let mut relay = bounded_relay(&fixture, 2);
    let stored = fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    deliver(&fixture, &mut relay, key);
    let checkpoint = relay.checkpoint().unwrap();
    let (mut restored, turn) = Relay::restore(
        fixture.identity(2),
        relay.limits,
        &Restore::restarted(Some(stored), Responsibility::Checkpoint(&checkpoint)),
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    let detached = work(&turn);
    let _ = complete(&mut restored, &detached, fixture.at);
    discard_outputs(&mut restored, turn, fixture.at);
    assert!(restored.connections.is_empty());
    let _ = restored.handle(Input::Closed(key.sender()), fixture.at);
    assert_eq!(
        restored
            .checkpoint_manifest()
            .unwrap()
            .generations()
            .collect::<Vec<_>>(),
        &[key.receiver()]
    );
    assert_eq!(
        restored.ledgers[&key.receiver()].usage().submitted,
        ONE_FRAME
    );
    assert_eq!(
        restored
            .handle(Input::Connected(connection(3)), fixture.at)
            .refusal,
        None
    );
    assert_eq!(
        restored
            .handle(Input::Connected(connection(4)), fixture.at)
            .refusal,
        Some(Error::Capacity)
    );
}

#[test]
fn old_generation_feedback_cannot_release_or_close_a_replacement_connection() {
    let fixture = Fixture::new();
    let mut relay = retirement::relay(&fixture, 2, 1, retirement::policy(2, Counts::new(2, 42)));
    let stored = fixture.register(&mut relay);
    let (old, _) = fixture.join(&mut relay, 2);
    deliver(&fixture, &mut relay, old);
    let detached = relay.handle(Input::Disconnected(old.receiver()), fixture.at);
    discard_outputs(&mut relay, detached, fixture.at);
    let checkpoint = relay.checkpoint().unwrap();
    let (mut relay, turn) = Relay::restore(
        fixture.identity(2),
        relay.limits,
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[old.sender()],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    discard_outputs(&mut relay, turn, fixture.at);
    let next_connection = ConnectionRef {
        generation: NonZeroU64::new(2).unwrap(),
        ..old.receiver()
    };
    let (next, _) = fixture.join_connection(&mut relay, next_connection);
    deliver(&fixture, &mut relay, next);
    let before = relay.ledgers[&next_connection].checkpoint().unwrap();
    let _ = relay.handle(
        Input::Settled {
            connection: next_connection,
            session: old.wire_id(),
            total: ONE_FRAME,
        },
        fixture.at,
    );
    assert_eq!(relay.ledgers[&old.receiver()].usage().submitted, ONE_FRAME);
    for _ in 0..2 {
        assert_eq!(
            relay
                .handle(
                    Input::Settled {
                        connection: old.receiver(),
                        session: old.wire_id(),
                        total: ONE_FRAME
                    },
                    fixture.at
                )
                .refusal,
            None
        );
        let _ = relay.handle(Input::Closed(old.receiver()), fixture.at);
        assert_eq!(
            relay.ledgers[&next_connection].checkpoint().unwrap(),
            before
        );
        assert!(matches!(
            relay.connections[&next_connection].role,
            Role::Established
        ));
    }
    assert!(!relay.ledgers.contains_key(&old.receiver()));
    assert_eq!(relay.connection_usage().retired_generations, 0);
    // Releasing an old generation cannot also release the replacement's admission slot.
    assert_eq!(
        relay
            .handle(Input::Connected(connection(3)), fixture.at)
            .refusal,
        Some(Error::Capacity)
    );
}

#[test]
fn absent_responsibility_never_means_a_fresh_budget() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    for record in [None, Some(stored)] {
        assert!(matches!(
            Relay::restore(
                fixture.identity(2),
                Fixture::limits(),
                &Restore::restarted(record, Responsibility::Unknown),
                Trace::default(),
                fixture.at,
            ),
            Err(Error::MissingResponsibility)
        ));
    }
    assert!(matches!(
        Relay::restore(
            fixture.identity(2),
            Fixture::limits(),
            &Restore {
                authority: Authority::Known(Some(stored)),
                surviving: &[connection(1)],
                responsibility: Responsibility::Released
            },
            Trace::default(),
            fixture.at,
        ),
        Err(Error::InvalidCheckpoint)
    ));
    let manifest = relay.checkpoint_manifest().unwrap();
    let partial = Checkpoint::from_connections(&manifest, Vec::new(), Fixture::limits()).unwrap();
    let (recovered, _) = Relay::restore(
        fixture.identity(2),
        Fixture::limits(),
        &Restore::restarted(Some(stored), Responsibility::Checkpoint(&partial)),
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert_eq!(
        recovered.unavailable_reservation(connection(1)),
        Some(Fixture::limits().delivery.total())
    );
}
