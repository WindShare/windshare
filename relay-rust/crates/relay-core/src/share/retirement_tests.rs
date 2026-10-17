use super::*;

const RECONNECTS: u64 = 6;
const GRACE_MS: u64 = 100;

pub(super) fn policy(generations: usize, residual: Counts) -> RetirementPolicy {
    RetirementPolicy::Bounded {
        grace: Duration::ZERO,
        generations,
        residual,
    }
}

pub(super) fn relay(
    fixture: &Fixture,
    connections: usize,
    sessions: usize,
    retirement: RetirementPolicy,
) -> Relay {
    let limits = Limits {
        connections,
        sessions,
        retained_flows_per_connection: sessions + 1,
        retirement,
        ..Fixture::limits()
    };
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

fn consume(relay: &mut Relay, key: SessionKey, frames: u64, at: UnixMillis) {
    assert_eq!(
        relay
            .handle(
                Input::Consumed {
                    connection: key.receiver(),
                    session: key.wire_id(),
                    total: Counts::new(frames, frames * ONE_FRAME.encoded_bytes),
                },
                at
            )
            .refusal,
        None
    );
}

fn download(relay: &mut Relay, key: SessionKey, flow: FlowKey, frames: u64, at: UnixMillis) {
    assert_eq!(
        relay
            .handle(
                Input::Grant {
                    flow,
                    credit: ONE_FRAME
                },
                at
            )
            .refusal,
        None
    );
    let receipt = receive_from(relay, key, key.sender(), at);
    assert_eq!(
        relay.submit(&receipt, at, |_| Submission::Complete).refusal,
        None
    );
    consume(relay, key, frames, at);
}

#[test]
fn repeated_reconnects_leave_healthy_download_and_session_admission_available() {
    let fixture = Fixture::new();
    let mut relay = relay(
        &fixture,
        3,
        2,
        policy(
            usize::try_from(RECONNECTS).unwrap(),
            Counts::new(RECONNECTS, RECONNECTS * ONE_FRAME.encoded_bytes),
        ),
    );
    fixture.register(&mut relay);
    let (healthy, healthy_flows) = fixture.join(&mut relay, 2);
    deliver(&fixture, &mut relay, healthy);
    consume(&mut relay, healthy, 1, fixture.at);
    for attempt in 0..RECONNECTS {
        let (lost, _) = fixture.join(&mut relay, attempt + 3);
        deliver(&fixture, &mut relay, lost);
        let healthy_ledger = relay.ledgers[&healthy.receiver()].checkpoint().unwrap();
        let lost_turn = relay.handle(Input::Disconnected(lost.receiver()), fixture.at);
        assert!(!lost_turn.outputs.iter().any(|output| matches!(
            output, Output::Close(target) if *target == healthy.receiver() || *target == healthy.sender()
        )));
        // Merely observing EOF has not confirmed either direction's application queue.
        assert_eq!(relay.connection_usage().admitted, 3);
        discard_outputs(&mut relay, lost_turn, fixture.at);
        assert_eq!(
            relay.connection_usage(),
            ConnectionUsage {
                uncertain: Counts::ZERO,
                admitted: 2,
                retired_generations: usize::try_from(attempt).unwrap() + 1,
                residual: Counts::new(attempt + 1, (attempt + 1) * ONE_FRAME.encoded_bytes),
            }
        );
        assert_eq!(
            relay.ledgers[&healthy.receiver()].checkpoint().unwrap(),
            healthy_ledger
        );
        assert_eq!(relay.sessions.len(), 1);
        download(
            &mut relay,
            healthy,
            healthy_flows[1],
            attempt + 2,
            fixture.at,
        );
    }
    // More old generations than the active connection/session limits remain bounded separately.
    let (replacement, _) = fixture.join(&mut relay, RECONNECTS + 3);
    deliver(&fixture, &mut relay, replacement);
    assert_eq!(relay.sessions.len(), 2);
    assert_eq!(
        relay.connection_usage().retired_generations,
        usize::try_from(RECONNECTS).unwrap()
    );
    assert_eq!(
        relay.ledgers[&healthy.sender()]
            .metadata_usage()
            .retained_flows,
        2
    );
}

#[test]
fn generation_frame_and_byte_residual_limits_each_block_only_further_admission() {
    let fixture = Fixture::new();
    for retirement in [
        policy(1, Counts::new(2, 42)),
        policy(2, Counts::new(1, 42)),
        policy(2, Counts::new(2, 21)),
    ] {
        let mut relay = relay(&fixture, 3, 2, retirement);
        fixture.register(&mut relay);
        let (healthy, healthy_flows) = fixture.join(&mut relay, 2);
        deliver(&fixture, &mut relay, healthy);
        let (first, _) = fixture.join(&mut relay, 3);
        deliver(&fixture, &mut relay, first);
        let turn = relay.handle(Input::Disconnected(first.receiver()), fixture.at);
        discard_outputs(&mut relay, turn, fixture.at);
        let (pending, _) = fixture.join(&mut relay, 4);
        deliver(&fixture, &mut relay, pending);
        let turn = relay.handle(Input::Disconnected(pending.receiver()), fixture.at);
        discard_outputs(&mut relay, turn, fixture.at);
        assert_eq!(
            relay.connection_usage(),
            ConnectionUsage {
                uncertain: Counts::ZERO,
                admitted: 3,
                retired_generations: 1,
                residual: ONE_FRAME,
            }
        );
        assert_eq!(
            relay
                .handle(Input::Connected(connection(5)), fixture.at)
                .refusal,
            Some(Error::Capacity)
        );
        assert!(relay.observer().0.iter().any(|event| matches!(
            event, Observation::Relay { connection: Some(target),
                decision: RelayDecision::RetirementBlocked { .. }, .. }
                if *target == pending.receiver()
        )));
        download(&mut relay, healthy, healthy_flows[1], 2, fixture.at);
        // Exact old-generation settlement opens residual space, then retries waiting retirement.
        assert_eq!(
            relay
                .handle(
                    Input::Settled {
                        connection: first.receiver(),
                        session: first.wire_id(),
                        total: ONE_FRAME,
                    },
                    fixture.at
                )
                .refusal,
            None
        );
        assert_eq!(
            relay.connection_usage(),
            ConnectionUsage {
                uncertain: Counts::ZERO,
                admitted: 2,
                retired_generations: 1,
                residual: ONE_FRAME,
            }
        );
        let (replacement, _) = fixture.join(&mut relay, 5);
        deliver(&fixture, &mut relay, replacement);
    }
}

#[test]
fn retirement_waits_for_both_application_queues_and_fences_delayed_submissions() {
    let fixture = Fixture::new();
    let mut relay = relay(&fixture, 2, 1, policy(1, ONE_FRAME));
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
    assert_eq!(
        relay.handle(Input::Discarded(flows[1]), fixture.at).refusal,
        None
    );
    assert_eq!(
        relay.ledgers[&key.receiver()].usage().received,
        Counts::ZERO
    );
    assert_eq!(
        relay
            .handle(Input::Connected(connection(3)), fixture.at)
            .refusal,
        Some(Error::Capacity)
    );
    for receipt in [to_sender, to_receiver] {
        assert!(
            relay
                .submit(&receipt, fixture.at, |_| panic!("revoked handoff"))
                .refusal
                .is_some()
        );
    }
    assert_eq!(
        relay.handle(Input::Discarded(flows[0]), fixture.at).refusal,
        None
    );
    assert_eq!(relay.connection_usage().retired_generations, 1);
    fixture.join(&mut relay, 3);
    assert_eq!(
        relay.delivery_usage(key.receiver()).unwrap().submitted,
        ONE_FRAME
    );
}

#[test]
fn physical_release_does_not_hide_queues_on_another_destination() {
    let fixture = Fixture::new();
    let mut relay = relay(&fixture, 2, 1, RetirementPolicy::PhysicalRelease);
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    let queued = receive(&mut relay, key, fixture.at);
    let _ = relay.handle(Input::Closed(key.receiver()), fixture.at);
    assert_eq!(
        relay.delivery_usage(key.receiver()).unwrap().total(),
        Counts::ZERO
    );
    assert_eq!(
        relay
            .handle(Input::Connected(connection(3)), fixture.at)
            .refusal,
        Some(Error::Capacity)
    );
    assert!(
        relay
            .submit(&queued, fixture.at, |_| panic!("closed source"))
            .refusal
            .is_some()
    );
    assert_eq!(
        relay.handle(Input::Discarded(flows[0]), fixture.at).refusal,
        None
    );
    assert!(relay.delivery_usage(key.receiver()).is_none());
    assert_eq!(relay.connection_usage().admitted, 1);
    fixture.join(&mut relay, 3);
}

#[test]
fn recovery_preserves_retirement_grace_and_never_treats_expiry_as_release() {
    let mut fixture = Fixture::new();
    let mut relay = relay(
        &fixture,
        2,
        1,
        RetirementPolicy::Bounded {
            grace: Duration::from_millis(GRACE_MS),
            generations: 2,
            residual: Counts::new(2, 42),
        },
    );
    let stored = fixture.register(&mut relay);
    let (lost, _) = fixture.join(&mut relay, 2);
    deliver(&fixture, &mut relay, lost);
    let turn = relay.handle(Input::Disconnected(lost.receiver()), fixture.at);
    discard_outputs(&mut relay, turn, fixture.at);
    let retirement_at = UnixMillis(fixture.at.0 + GRACE_MS);
    let settlement_at = UnixMillis(fixture.at.0 + SETTLEMENT_BUDGET);
    for incarnation in 2..=4 {
        let now = UnixMillis(fixture.at.0 + u64::from(incarnation));
        let checkpoint =
            Checkpoint::decode(&relay.checkpoint().unwrap().encode(), relay.limits).unwrap();
        let (mut restored, turn) = Relay::restore(
            fixture.identity(incarnation),
            relay.limits,
            &Restore {
                authority: Authority::Known(Some(stored)),
                surviving: &[lost.sender()],
                responsibility: Responsibility::Checkpoint(&checkpoint),
            },
            Trace::default(),
            now,
        )
        .unwrap();
        discard_outputs(&mut restored, turn, now);
        let _ = restored.handle(Input::Disconnected(lost.receiver()), now);
        assert_eq!(restored.deadline(), Some(retirement_at));
        assert_eq!(
            restored
                .handle(Input::Connected(connection(3)), now)
                .refusal,
            Some(Error::Capacity)
        );
        relay = restored;
    }
    let _ = relay.handle(Input::Tick, retirement_at);
    assert_eq!(
        relay.connection_usage(),
        ConnectionUsage {
            uncertain: Counts::ZERO,
            admitted: 1,
            retired_generations: 1,
            residual: ONE_FRAME,
        }
    );
    let checkpoint =
        Checkpoint::decode(&relay.checkpoint().unwrap().encode(), relay.limits).unwrap();
    let (mut relay, turn) = Relay::restore(
        fixture.identity(5),
        relay.limits,
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[lost.sender()],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        retirement_at,
    )
    .unwrap();
    discard_outputs(&mut relay, turn, retirement_at);
    assert_eq!(relay.deadline(), Some(settlement_at));
    fixture.at = retirement_at;
    let (replacement, _) = fixture.join(&mut relay, 3);
    deliver(&fixture, &mut relay, replacement);
    let expired = relay.handle(Input::Tick, settlement_at);
    assert!(
        !expired
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(_)))
    );
    assert_eq!(
        relay.delivery_usage(lost.receiver()).unwrap().submitted,
        ONE_FRAME
    );
    assert_eq!(relay.connection_usage().retired_generations, 1);
    assert_eq!(
        relay.deadline(),
        Some(
            fixture
                .at
                .saturating_add(crate::heartbeat::CONNECTION_SILENCE_TIMEOUT)
        )
    );
    assert!(relay.ledgers[&lost.receiver()].deadline().is_none());
}

#[test]
fn retired_session_releases_admission_but_healthy_sender_keeps_its_flow_debt() {
    let fixture = Fixture::new();
    let mut relay = relay(&fixture, 3, 2, policy(2, Counts::new(2, 42)));
    fixture.register(&mut relay);
    let (healthy, healthy_flows) = fixture.join(&mut relay, 2);
    deliver(&fixture, &mut relay, healthy);
    let mut old = Vec::new();
    for receiver in [3, 4] {
        let (key, _) = fixture.join(&mut relay, receiver);
        let request = receive(&mut relay, key, fixture.at);
        assert_eq!(
            relay
                .submit(&request, fixture.at, |_| Submission::Complete)
                .refusal,
            None
        );
        let turn = relay.handle(Input::Disconnected(key.receiver()), fixture.at);
        discard_outputs(&mut relay, turn, fixture.at);
        old.push(key);
    }
    assert_eq!(relay.sessions.len(), 1);
    assert_eq!(
        relay.ledgers[&healthy.sender()]
            .metadata_usage()
            .retained_flows,
        3
    );
    assert_eq!(
        relay.ledgers[&healthy.sender()].usage().submitted,
        Counts::new(2, 42)
    );
    assert_eq!(
        relay
            .handle(Input::Connected(connection(5)), fixture.at)
            .refusal,
        None
    );
    let refused = fixture.control(&mut relay, connection(5), "joinB64", fixture.at);
    assert!(!refused.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Descriptor { .. } | Control::Grant(_),
            ..
        }
    )));
    download(&mut relay, healthy, healthy_flows[1], 2, fixture.at);
    let _ = relay.handle(Input::Closed(connection(5)), fixture.at);
    assert_eq!(
        relay
            .handle(
                Input::Settled {
                    connection: healthy.sender(),
                    session: old[0].wire_id(),
                    total: ONE_FRAME,
                },
                fixture.at
            )
            .refusal,
        None
    );
    fixture.join(&mut relay, 6);
    assert_eq!(
        relay.ledgers[&healthy.sender()].usage().submitted,
        ONE_FRAME
    );
}

#[test]
fn retired_checkpoints_cannot_hide_excess_risk_or_live_authority() {
    let fixture = Fixture::new();
    let mut relay = relay(&fixture, 2, 1, policy(2, Counts::new(2, 42)));
    let stored = fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    deliver(&fixture, &mut relay, key);
    let live = relay.checkpoint().unwrap();
    let turn = relay.handle(Input::Disconnected(key.receiver()), fixture.at);
    discard_outputs(&mut relay, turn, fixture.at);
    let checkpoint = relay.checkpoint().unwrap();
    for retirement in [
        RetirementPolicy::PhysicalRelease,
        policy(0, ONE_FRAME),
        policy(1, Counts::new(0, ONE_FRAME.encoded_bytes)),
        policy(1, Counts::new(1, ONE_FRAME.encoded_bytes - 1)),
    ] {
        assert!(matches!(
            Relay::restore(
                fixture.identity(2),
                Limits {
                    retirement,
                    ..relay.limits
                },
                &Restore {
                    authority: Authority::Known(Some(stored)),
                    surviving: &[key.sender()],
                    responsibility: Responsibility::Checkpoint(&checkpoint),
                },
                Trace::default(),
                fixture.at,
            ),
            Err(Error::InvalidCheckpoint)
        ));
    }
    let mut forged = live;
    let receiver = forged
        .connections
        .iter_mut()
        .find(|entry| entry.connection == key.receiver())
        .unwrap();
    receiver.role = checkpoint::ConnectionRole::Detached(Detached::Retired);
    receiver.sessions.clear();
    assert!(matches!(
        Relay::restore(
            fixture.identity(2),
            relay.limits,
            &Restore {
                authority: Authority::Known(Some(stored)),
                surviving: &[key.sender()],
                responsibility: Responsibility::Checkpoint(&forged),
            },
            Trace::default(),
            fixture.at,
        ),
        Err(Error::InvalidCheckpoint)
    ));
}
