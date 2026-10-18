use super::*;
use crate::share::{AttemptLimiter, AttemptRate, HandshakeLimits, admission::CONTROL_LIFETIME};
use std::collections::VecDeque;

fn bounded(fixture: &Fixture, business: usize, controls: usize) -> Relay {
    let limits = Limits {
        business_connections: business,
        handshakes: HandshakeLimits {
            concurrent: controls,
            ..Fixture::limits().handshakes
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

fn finish_stop(relay: &mut Relay, initial: Turn, now: UnixMillis) -> bool {
    let mut pending: VecDeque<_> = initial.outputs.into();
    let mut acknowledged = false;
    while let Some(output) = pending.pop_front() {
        let next = match output {
            Output::Work(task) => complete(relay, &task, now),
            Output::Discard(flow) => relay.handle(Input::Discarded(flow), now),
            Output::Send {
                control: Control::Stopped(_),
                ..
            } => {
                assert_eq!(relay.lifecycle.phase(), Phase::Stopped);
                acknowledged = true;
                Turn::default()
            }
            _ => Turn::default(),
        };
        pending.extend(next.outputs);
    }
    acknowledged
}

#[test]
fn full_business_budget_allows_authenticated_stop_without_refunding_delivery() {
    let fixture = Fixture::new();
    let mut relay = bounded(&fixture, 2, 1);
    fixture.register(&mut relay);
    let (session, flows) = fixture.join(&mut relay, 2);
    submit_frames(&mut relay, session, flows[0], 1, fixture.at);
    assert_eq!(relay.connection_usage().business, 2);
    for (index, field) in ["joinB64", "registerInitB64", "resumeInitB64"]
        .into_iter()
        .enumerate()
    {
        let candidate = connection(10 + index as u64);
        assert_eq!(
            relay
                .handle(Input::Connected(candidate), fixture.at)
                .refusal,
            None
        );
        let refusal = fixture.control(&mut relay, candidate, field, fixture.at);
        assert_eq!(refusal.refusal, Some(Error::Capacity));
        assert!(
            !refusal
                .outputs
                .iter()
                .any(|output| matches!(output, Output::ChallengeNeeded(_) | Output::Work(_)))
        );
        let _ = relay.handle(Input::Closed(candidate), fixture.at);
    }
    fixture.prepare_stop(&mut relay);
    assert_eq!(relay.connection_usage().controls, 1);
    assert_eq!(relay.connection_usage().business, 2);
    assert!(relay.generations.ledger(&connection(99)).is_none());
    let stop = fixture.stop(&mut relay);
    assert!(finish_stop(&mut relay, stop, fixture.at));
    assert_eq!(
        ledger_of(&relay, session.sender()).usage().submitted,
        ONE_FRAME
    );
    assert_eq!(relay.connection_usage().business, 2);
    assert!(matches!(
        relay.generations.connection(&connection(99)).unwrap().role,
        Role::Closing
    ));
    let _ = relay.handle(Input::Closed(connection(99)), fixture.at);
    assert_eq!(relay.connection_usage().controls, 0);
}

#[test]
fn hibernation_preserves_stop_capacity_beside_unreleased_business_generations() {
    let fixture = Fixture::new();
    let mut relay = bounded(&fixture, 2, 1);
    let stored = fixture.register(&mut relay);
    let (session, flows) = fixture.join(&mut relay, 2);
    submit_frames(&mut relay, session, flows[0], 1, fixture.at);
    let detached = relay.handle(Input::Disconnected(session.receiver()), fixture.at);
    for output in detached.outputs {
        if let Output::Discard(flow) = output {
            let _ = relay.handle(Input::Discarded(flow), fixture.at);
        }
    }
    fixture.prepare_stop(&mut relay);
    let deadline = relay.generations.control_deadline(&connection(99)).unwrap();
    let checkpoint =
        Checkpoint::decode(&relay.checkpoint().unwrap().encode(), relay.limits).unwrap();
    let (mut restored, _) = Relay::restore(
        fixture.identity(2),
        relay.limits,
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[session.sender(), connection(99)],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert_eq!(restored.connection_usage().business, 2);
    assert_eq!(restored.connection_usage().controls, 1);
    assert_eq!(
        restored
            .generations
            .control_deadline(&connection(99))
            .unwrap(),
        deadline
    );
    let stop = fixture.stop(&mut restored);
    assert!(finish_stop(&mut restored, stop, fixture.at));
    assert_eq!(
        ledger_of(&restored, session.sender()).usage().submitted,
        ONE_FRAME
    );
}

#[test]
fn reserved_control_capacity_still_requires_a_valid_sender_proof() {
    let fixture = Fixture::new();
    let mut relay = bounded(&fixture, 1, 1);
    fixture.register(&mut relay);
    fixture.prepare_stop(&mut relay);
    let mut invalid = fixture.frame("stopProofB64");
    *invalid.last_mut().unwrap() ^= 1;
    let refused = relay.handle(
        Input::Control {
            connection: connection(99),
            frame: &invalid,
        },
        fixture.at,
    );
    assert!(
        !refused
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Work(_)))
    );
    assert_eq!(relay.lifecycle.phase(), Phase::Published);
    assert_eq!(relay.connection_usage().business, 1);
    let _ = relay.handle(Input::Closed(connection(99)), fixture.at);
    fixture.prepare_stop_on(&mut relay, connection(100));
    let stop = fixture.control(&mut relay, connection(100), "stopProofB64", fixture.at);
    assert!(finish_stop(&mut relay, stop, fixture.at));
}

#[test]
fn control_concurrency_and_attempt_rate_remain_bounded_across_reconstruction() {
    let fixture = Fixture::new();
    let mut limits = Fixture::limits();
    limits.handshakes = HandshakeLimits {
        concurrent: 1,
        attempts: AttemptRate {
            burst: 2,
            refill: Duration::from_millis(100),
        },
    };
    let (mut relay, _) = Relay::restore(
        fixture.identity(1),
        limits,
        &Restore::restarted(None, Responsibility::Released { previous: None }),
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert_eq!(
        relay
            .handle(Input::Connected(connection(1)), fixture.at)
            .refusal,
        None
    );
    assert_eq!(
        relay
            .handle(Input::Connected(connection(2)), fixture.at)
            .refusal,
        Some(Error::Capacity)
    );
    let _ = relay.handle(Input::Closed(connection(1)), fixture.at);
    assert_eq!(
        relay
            .handle(Input::Connected(connection(2)), fixture.at)
            .refusal,
        None
    );
    let _ = relay.handle(Input::Closed(connection(2)), fixture.at);
    let checkpoint = Checkpoint::decode(&relay.checkpoint().unwrap().encode(), limits).unwrap();
    let (mut relay, _) = Relay::restore(
        fixture.identity(2),
        limits,
        &Restore::restarted(None, Responsibility::Checkpoint(&checkpoint)),
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert_eq!(
        relay
            .handle(
                Input::Connected(connection(3)),
                UnixMillis(fixture.at.0 + 99)
            )
            .refusal,
        Some(Error::HandshakeRate)
    );
    assert_eq!(
        relay
            .handle(
                Input::Connected(connection(4)),
                UnixMillis(fixture.at.0 + 100)
            )
            .refusal,
        None
    );
    assert_eq!(relay.connection_usage().business, 0);
}

#[test]
fn a_control_handshake_does_not_renew_its_first_frame_deadline_on_hibernation() {
    let fixture = Fixture::new();
    let mut relay = bounded(&fixture, 1, 1);
    let _ = relay.handle(Input::Connected(connection(1)), fixture.at);
    let deadline = relay.deadline().unwrap();
    let checkpoint = relay.checkpoint().unwrap();
    let (mut relay, _) = Relay::restore(
        fixture.identity(2),
        relay.limits,
        &Restore {
            authority: Authority::Known(None),
            surviving: &[connection(1)],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        UnixMillis(deadline.0 - 1),
    )
    .unwrap();
    assert_eq!(relay.deadline(), Some(deadline));
    let expired = relay.handle(Input::Tick, deadline);
    assert!(
        expired
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(target) if *target == connection(1)))
    );
    let _ = relay.handle(Input::Disconnected(connection(1)), deadline);
    assert_eq!(relay.connection_usage().controls, 0);
    assert_eq!(
        relay
            .handle(Input::Connected(connection(2)), deadline)
            .refusal,
        None
    );
}

#[test]
fn pending_stop_storage_cannot_hold_a_control_socket_indefinitely() {
    let fixture = Fixture::new();
    let mut relay = bounded(&fixture, 1, 1);
    fixture.register(&mut relay);
    fixture.prepare_stop(&mut relay);
    let deadline = relay.generations.control_deadline(&connection(99)).unwrap();
    let initial = fixture.stop(&mut relay);
    let expired = relay.handle(Input::Tick, deadline);
    assert!(
        expired
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(target) if *target == connection(99)))
    );
    let _ = relay.handle(Input::Disconnected(connection(99)), deadline);
    assert_eq!(relay.connection_usage().controls, 0);
    assert!(!finish_stop(&mut relay, initial, deadline));
    assert_eq!(relay.lifecycle.phase(), Phase::Stopped);
    assert_eq!(
        relay
            .handle(Input::Connected(connection(100)), deadline)
            .refusal,
        None
    );
}

#[test]
fn lost_control_records_never_create_payload_debt_and_terminal_sockets_are_reclosed() {
    let fixture = Fixture::new();
    let mut relay = bounded(&fixture, 1, 1);
    let _ = relay.handle(Input::Connected(connection(1)), fixture.at);
    let manifest = relay.checkpoint_manifest().unwrap();
    let missing = Checkpoint::from_connections(&manifest, Vec::new(), relay.limits).unwrap();
    let (restored, turn) = Relay::restore(
        fixture.identity(2),
        relay.limits,
        &Restore {
            authority: Authority::Known(None),
            surviving: &[connection(1)],
            responsibility: Responsibility::Checkpoint(&missing),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert!(
        turn.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(target) if *target == connection(1)))
    );
    assert_eq!(restored.connection_usage(), ConnectionUsage::default());
    relay.close(connection(1), &mut Turn::default());
    let checkpoint = relay.checkpoint().unwrap();
    let (_, turn) = Relay::restore(
        fixture.identity(2),
        relay.limits,
        &Restore {
            authority: Authority::Known(None),
            surviving: &[connection(1)],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert!(
        turn.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(target) if *target == connection(1)))
    );
}

#[test]
fn rate_refill_is_bounded_and_backward_time_never_refunds_attempts() {
    let mut rate = AttemptLimiter::new(
        AttemptRate {
            burst: 2,
            refill: Duration::from_millis(100),
        },
        UnixMillis(100),
    )
    .unwrap();
    assert!(rate.attempt(UnixMillis(100)));
    assert!(rate.attempt(UnixMillis(100)));
    assert!(!rate.attempt(UnixMillis(0)));
    assert!(!rate.attempt(UnixMillis(199)));
    assert!(rate.attempt(UnixMillis(200)));
    assert!(!rate.attempt(UnixMillis(200)));
    assert!(rate.attempt(UnixMillis(u64::MAX)));
    assert!(rate.attempt(UnixMillis(u64::MAX)));
    assert!(!rate.attempt(UnixMillis(u64::MAX)));
    for attempts in [
        AttemptRate {
            burst: 0,
            refill: CONTROL_LIFETIME,
        },
        AttemptRate {
            burst: 1,
            refill: Duration::ZERO,
        },
        AttemptRate {
            burst: 1,
            refill: Duration::MAX,
        },
    ] {
        assert!(AttemptLimiter::new(attempts, UnixMillis(0)).is_err());
    }
}
