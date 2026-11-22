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
        None,
        Trace::default(),
        fixture.at,
    )
    .unwrap()
    .0
}

fn finish_stop(relay: &mut Relay, initial: RelayStep, now: UnixMillis) -> bool {
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
                RelayStep::default()
            }
            _ => RelayStep::default(),
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
        relay
            .generations
            .connection(&connection(99))
            .unwrap()
            .phase(),
        ConnectionPhase::Closing
    ));
    let _ = relay.handle(Input::Closed(connection(99)), fixture.at);
    assert_eq!(relay.connection_usage().controls, 0);
}

#[cfg(feature = "hibernation")]
#[test]
fn hibernation_preserves_stop_capacity_beside_saturated_business_connections() {
    let fixture = Fixture::new();
    let mut relay = bounded(&fixture, 2, 1);
    let stored = fixture.register(&mut relay);
    let (session, flows) = fixture.join(&mut relay, 2);
    submit_frames(&mut relay, session, flows[0], 1, fixture.at);
    let snapshots = snapshot_all(&relay);
    let (mut restored, _) = Relay::restore_hibernated(
        fixture.identity(2),
        relay.limits,
        &Hibernated {
            record: Some(stored),
            last_session: relay.last_session(),
            surviving: &[session.sender(), session.receiver()],
            connections: &snapshots,
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert_eq!(restored.connection_usage().business, 2);
    assert_eq!(restored.connection_usage().controls, 0);
    assert_business_capacity_refused(&fixture, &mut restored, connection(10), fixture.at);
    fixture.prepare_stop(&mut restored);
    assert_eq!(restored.connection_usage().controls, 1);
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
fn control_concurrency_and_attempt_rate_remain_bounded() {
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
        None,
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

#[test]
fn attempt_reserve_protects_burst_headroom_and_refills() {
    let mut rate = AttemptLimiter::new(
        AttemptRate {
            burst: 4,
            refill: Duration::from_millis(100),
        },
        UnixMillis(100),
    )
    .unwrap();
    assert_eq!(rate.burst(), 4);
    assert!(!rate.attempt_with_reserve(UnixMillis(100), 4));
    assert!(!rate.attempt_with_reserve(UnixMillis(100), u32::MAX));
    assert!(rate.attempt_with_reserve(UnixMillis(100), 2));
    assert!(rate.attempt_with_reserve(UnixMillis(100), 2));
    assert!(!rate.attempt_with_reserve(UnixMillis(100), 2));
    assert!(rate.attempt_with_reserve(UnixMillis(100), 1));
    assert!(!rate.attempt_with_reserve(UnixMillis(100), 1));
    assert!(rate.attempt_with_reserve(UnixMillis(100), 0));
    assert!(!rate.attempt_with_reserve(UnixMillis(100), 0));
    // One refill stays exclusively available to reserve=0 callers even if lower
    // priority callers probe first.
    assert!(!rate.attempt_with_reserve(UnixMillis(200), 2));
    assert!(!rate.attempt_with_reserve(UnixMillis(200), 1));
    assert!(rate.attempt_with_reserve(UnixMillis(200), 0));
    // Backwards time cannot replenish reserved tokens.
    assert!(!rate.attempt_with_reserve(UnixMillis(50), 0));
}
