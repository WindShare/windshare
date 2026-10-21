use super::*;
use crate::heartbeat::{CONNECTION_PROBE_INTERVAL, CONNECTION_SILENCE_TIMEOUT, ConnectionProbe};

fn probe(relay: &mut Relay, connection: ConnectionRef, nonce: u64, now: UnixMillis) -> RelayStep {
    relay.handle(
        Input::Control {
            connection,
            frame: &ConnectionProbe::new(NonZeroU64::new(nonce).unwrap()).encode(),
        },
        now,
    )
}

fn assert_ack(step: &RelayStep, connection: ConnectionRef, nonce: u64) {
    assert_eq!(step.refusal, None, "{step:?}");
    assert!(matches!(step.outputs.as_slice(), [Output::Send {
        connection: destination, control: Control::ProbeAck(ack),
    }] if *destination == connection && ack.nonce() == nonce));
}

fn deadline(relay: &Relay, connection: ConnectionRef) -> UnixMillis {
    let Some(liveness) = relay
        .generations
        .connection(&connection)
        .unwrap()
        .liveness()
    else {
        panic!("expected an established connection");
    };
    liveness.deadline()
}

fn active(relay: &mut Relay, fixture: &Fixture, receiver: u64) -> (SessionKey, [FlowKey; 2]) {
    let (key, flows) = fixture.join(relay, receiver);
    let receipt = receive(relay, key, fixture.at);
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    activate(relay, key, fixture.at);
    (key, flows)
}

#[test]
fn idle_connections_only_reply_to_client_probes_and_expire_on_silence() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let sender = connection(1);
    let mut at = fixture.at;
    for nonce in 1..=4 {
        at = at.saturating_add(CONNECTION_PROBE_INTERVAL);
        assert!(relay.handle(Input::Tick, at).outputs.is_empty());
        let step = probe(&mut relay, sender, nonce, at);
        assert_ack(&step, sender, nonce);
        assert_eq!(
            step.deadline,
            Some(at.saturating_add(CONNECTION_SILENCE_TIMEOUT))
        );
    }
    let expires = deadline(&relay, sender);
    assert!(
        relay
            .handle(Input::Tick, UnixMillis(expires.0 - 1))
            .outputs
            .is_empty()
    );
    let step = probe(&mut relay, sender, 5, expires);
    assert_eq!(step.refusal, Some(Error::Unavailable));
    assert!(
        step.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == sender))
    );
    assert!(!step.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::ProbeAck(_),
            ..
        }
    )));
    assert!(relay.observer().0.iter().any(|event| matches!(event, Observation::Relay {
        connection: Some(value), decision: RelayDecision::ConnectionSilent {last_probe_nonce: 4, last_inbound, deadline}, ..
    } if *value == sender && *last_inbound == at && *deadline == expires)));
}

#[test]
fn probes_bypass_exhausted_credit_and_unsubmitted_content() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    let _receipt = receive(&mut relay, key, fixture.at);
    let sender_usage = relay.delivery_usage(key.sender());
    let receiver_usage = relay.delivery_usage(key.receiver());
    let admission = relay.sessions.entries()[&key.wire_id()].session.deadline();
    let at = UnixMillis(fixture.at.0 + 1);
    for peer in [key.sender(), key.receiver()] {
        assert_ack(&probe(&mut relay, peer, 1, at), peer, 1);
    }
    assert_eq!(relay.delivery_usage(key.sender()), sender_usage);
    assert_eq!(relay.delivery_usage(key.receiver()), receiver_usage);
    assert_eq!(
        relay.sessions.entries()[&key.wire_id()].session.deadline(),
        admission
    );
}

#[test]
fn accepted_content_admission_and_feedback_refresh_only_the_inbound_connection() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    let first = UnixMillis(fixture.at.0 + 1);
    let receipt = receive(&mut relay, key, first);
    assert_eq!(
        deadline(&relay, key.receiver()),
        first.saturating_add(CONNECTION_SILENCE_TIMEOUT)
    );
    assert_eq!(
        deadline(&relay, key.sender()),
        fixture.at.saturating_add(CONNECTION_SILENCE_TIMEOUT)
    );
    assert_eq!(
        relay
            .submit(&receipt, first, |_| Submission::Complete)
            .refusal,
        None
    );
    let admitted = UnixMillis(first.0 + 1);
    activate(&mut relay, key, admitted);
    assert_eq!(
        deadline(&relay, key.sender()),
        admitted.saturating_add(CONNECTION_SILENCE_TIMEOUT)
    );
    let data_at = fixture.at.saturating_add(CONNECTION_PROBE_INTERVAL);
    let receipt = receive_from(&mut relay, key, key.sender(), data_at);
    assert_eq!(
        relay
            .submit(&receipt, data_at, |_| Submission::Complete)
            .refusal,
        None
    );
    assert_eq!(
        deadline(&relay, key.sender()),
        data_at.saturating_add(CONNECTION_SILENCE_TIMEOUT)
    );
    assert_eq!(
        deadline(&relay, key.receiver()),
        first.saturating_add(CONNECTION_SILENCE_TIMEOUT)
    );
    let consumed_at = data_at.saturating_add(CONNECTION_PROBE_INTERVAL);
    let feedback = crate::flow::DeliveryConsumed::new(key.wire_id(), Counts::new(1, 21)).unwrap();
    assert_eq!(
        relay
            .handle(
                Input::Control {
                    connection: key.receiver(),
                    frame: &feedback.encode()
                },
                consumed_at
            )
            .refusal,
        None
    );
    assert_eq!(
        deadline(&relay, key.receiver()),
        consumed_at.saturating_add(CONNECTION_SILENCE_TIMEOUT)
    );
    assert!(
        relay
            .handle(
                Input::Tick,
                fixture.at.saturating_add(CONNECTION_SILENCE_TIMEOUT)
            )
            .outputs
            .is_empty()
    );
    assert_eq!(
        relay.sessions.entries()[&key.wire_id()].session.phase(),
        crate::sessions::Phase::Active
    );
}

#[test]
fn nonce_validation_is_strict_connection_local_and_does_not_wrap() {
    for rejected in [1, 2] {
        let fixture = Fixture::new();
        let mut relay = fixture.relay();
        fixture.register(&mut relay);
        let (key, _) = active(&mut relay, &fixture, 2);
        assert_ack(
            &probe(&mut relay, key.sender(), 2, fixture.at),
            key.sender(),
            2,
        );
        assert_ack(
            &probe(&mut relay, key.receiver(), 1, fixture.at),
            key.receiver(),
            1,
        );
        let step = probe(&mut relay, key.receiver(), rejected, fixture.at);
        if rejected == 1 {
            assert_eq!(step.refusal, Some(Error::ProbeNonce));
            assert!(
                step.outputs.iter().any(
                    |output| matches!(output, Output::Close(value) if *value == key.receiver())
                )
            );
            assert!(matches!(
                relay.generations.connection(&key.sender()).unwrap().phase(),
                ConnectionPhase::Established
            ));
        } else {
            assert_ack(&step, key.receiver(), rejected);
            assert_ack(
                &probe(&mut relay, key.receiver(), u64::MAX, fixture.at),
                key.receiver(),
                u64::MAX,
            );
            assert_eq!(
                probe(&mut relay, key.receiver(), 1, fixture.at).refusal,
                Some(Error::ProbeNonce)
            );
        }
    }
}

#[test]
fn malformed_probes_and_wrong_direction_acknowledgements_close_without_a_reply() {
    let valid = ConnectionProbe::new(NonZeroU64::new(1).unwrap());
    let mut zero = valid.encode();
    zero[8..].fill(0);
    for frame in [
        zero.as_slice(),
        &valid.encode()[..15],
        &valid.acknowledgement().encode(),
    ] {
        let fixture = Fixture::new();
        let mut relay = fixture.relay();
        fixture.register(&mut relay);
        let step = relay.handle(
            Input::Control {
                connection: connection(1),
                frame,
            },
            fixture.at,
        );
        assert!(step.refusal.is_some());
        assert!(
            step.outputs
                .iter()
                .any(|output| matches!(output, Output::Close(_)))
        );
        assert!(!step.outputs.iter().any(|output| matches!(
            output,
            Output::Send {
                control: Control::ProbeAck(_),
                ..
            }
        )));
    }
}

#[test]
fn pre_handshake_probes_cannot_create_an_established_connection() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let peer = connection(1);
    assert_eq!(
        relay.handle(Input::Connected(peer), fixture.at).refusal,
        None
    );
    let step = probe(&mut relay, peer, 1, UnixMillis(fixture.at.0 + 1));
    assert!(
        step.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == peer))
    );
    assert!(!step.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::ProbeAck(_),
            ..
        }
    )));
    assert_eq!(relay.deadline(), None);
}

#[test]
fn replacement_generation_starts_its_own_nonce_without_accepting_old_probes() {
    let mut fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, _) = active(&mut relay, &fixture, 2);
    assert_ack(
        &probe(&mut relay, key.receiver(), 7, fixture.at),
        key.receiver(),
        7,
    );
    let _ = relay.handle(Input::Disconnected(key.receiver()), fixture.at);
    fixture.at = UnixMillis(fixture.at.0 + 1);
    let replacement = ConnectionRef {
        generation: NonZeroU64::new(2).unwrap(),
        ..key.receiver()
    };
    fixture.join_connection(&mut relay, replacement);
    assert_ack(
        &probe(&mut relay, replacement, 1, fixture.at),
        replacement,
        1,
    );
    let original = deadline(&relay, replacement);
    let late = probe(&mut relay, key.receiver(), 8, UnixMillis(fixture.at.0 + 1));
    assert_eq!(late.refusal, Some(Error::UnknownConnection));
    assert!(late.outputs.is_empty());
    assert_eq!(deadline(&relay, replacement), original);
}

#[test]
fn synchronous_handoff_rechecks_silence_before_invoking_the_host() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, _) = active(&mut relay, &fixture, 2);
    let receipt = receive_from(&mut relay, key, key.sender(), UnixMillis(fixture.at.0 + 1));
    let step = relay.submit(
        &receipt,
        fixture.at.saturating_add(CONNECTION_SILENCE_TIMEOUT),
        |_| panic!("silent destination submitted"),
    );
    assert!(step.refusal.is_some());
    assert!(
        step.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == key.receiver()))
    );
    assert!(
        !step
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == key.sender()))
    );
}

#[test]
fn ignored_feedback_host_callbacks_and_rejected_frames_do_not_refresh_liveness() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, flows) = active(&mut relay, &fixture, 2);
    let original = deadline(&relay, key.sender());
    let at = fixture.at.saturating_add(CONNECTION_PROBE_INTERVAL);
    let unknown = crate::identifiers::RelaySessionId::new(u64::MAX.to_be_bytes()).unwrap();
    assert_eq!(
        relay
            .handle(
                Input::Consumed {
                    connection: key.sender(),
                    session: unknown,
                    total: Counts::ZERO
                },
                at
            )
            .refusal,
        None
    );
    assert_eq!(
        relay
            .handle(
                Input::Grant {
                    flow: flows[1],
                    credit: Counts::new(1, 21)
                },
                at
            )
            .refusal,
        None
    );
    assert!(
        relay
            .handle(
                Input::Consumed {
                    connection: key.sender(),
                    session: key.wire_id(),
                    total: Counts::new(2, 42)
                },
                at
            )
            .refusal
            .is_some()
    );
    assert_eq!(deadline(&relay, key.sender()), original);
    let invalid = relay.handle(
        Input::Control {
            connection: key.sender(),
            frame: b"WS2B",
        },
        at,
    );
    assert!(invalid.refusal.is_some());
    assert_eq!(deadline(&relay, key.sender()), original);
}

#[test]
fn a_silent_receiver_loses_authority_without_refunding_debt_or_closing_siblings() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (silent, _) = active(&mut relay, &fixture, 2);
    let (healthy, _) = active(&mut relay, &fixture, 3);
    let receipt = receive_from(&mut relay, silent, silent.sender(), fixture.at);
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    let before = relay.delivery_usage(silent.receiver()).unwrap();
    let at = fixture.at.saturating_add(CONNECTION_PROBE_INTERVAL);
    for peer in [healthy.sender(), healthy.receiver()] {
        assert_ack(&probe(&mut relay, peer, 1, at), peer, 1);
    }
    let step = relay.handle(
        Input::Tick,
        fixture.at.saturating_add(CONNECTION_SILENCE_TIMEOUT),
    );
    let closed: Vec<_> = step
        .outputs
        .iter()
        .filter_map(|output| match output {
            Output::Close(value) => Some(*value),
            _ => None,
        })
        .collect();
    assert_eq!(closed, [silent.receiver()]);
    assert_eq!(
        relay.sessions.entries()[&healthy.wire_id()].session.phase(),
        crate::sessions::Phase::Active
    );
    assert_eq!(
        relay.delivery_usage(silent.receiver()).unwrap().submitted,
        before.submitted
    );
    assert!(
        relay
            .generations
            .retirement_state(&silent.receiver())
            .is_none()
    );
    assert!(
        relay
            .submit(
                &receipt,
                fixture.at.saturating_add(CONNECTION_SILENCE_TIMEOUT),
                |_| panic!("revoked permit submitted")
            )
            .refusal
            .is_some()
    );
}

#[test]
fn repeated_hibernation_preserves_original_expiry_nonce_and_delivery_debt() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let (key, _) = active(&mut relay, &fixture, 2);
    let at = fixture.at.saturating_add(CONNECTION_PROBE_INTERVAL);
    for peer in [key.sender(), key.receiver()] {
        assert_ack(&probe(&mut relay, peer, 7, at), peer, 7);
    }
    let expires = at.saturating_add(CONNECTION_SILENCE_TIMEOUT);
    let usage = relay.delivery_usage(key.sender());
    for incarnation in 2..=3 {
        let checkpoint =
            Checkpoint::decode(&relay.checkpoint().unwrap().encode(), Fixture::limits()).unwrap();
        let (restored, step) = Relay::restore(
            fixture.identity(incarnation),
            Fixture::limits(),
            &Restore {
                authority: Authority::Known(Some(stored)),
                surviving: &[key.sender(), key.receiver()],
                responsibility: Responsibility::Checkpoint(&checkpoint),
            },
            Trace::default(),
            UnixMillis(expires.0 - u64::from(4 - incarnation)),
        )
        .unwrap();
        assert!(step.outputs.is_empty());
        assert_eq!(step.deadline, Some(expires));
        assert_eq!(restored.delivery_usage(key.sender()), usage);
        relay = restored;
    }
    let checkpoint =
        Checkpoint::decode(&relay.checkpoint().unwrap().encode(), Fixture::limits()).unwrap();
    assert_eq!(
        probe(&mut relay, key.receiver(), 7, UnixMillis(expires.0 - 1)).refusal,
        Some(Error::ProbeNonce)
    );
    let (mut resumed, _) = Relay::restore(
        fixture.identity(4),
        Fixture::limits(),
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[key.sender(), key.receiver()],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        UnixMillis(expires.0 - 1),
    )
    .unwrap();
    let received_at = UnixMillis(expires.0 - 1);
    assert_ack(
        &probe(&mut resumed, key.receiver(), 8, received_at),
        key.receiver(),
        8,
    );
    assert_eq!(
        deadline(&resumed, key.receiver()),
        received_at.saturating_add(CONNECTION_SILENCE_TIMEOUT)
    );
    let (restored, step) = Relay::restore(
        fixture.identity(4),
        Fixture::limits(),
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[key.sender(), key.receiver()],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        expires,
    )
    .unwrap();
    assert!(
        step.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == key.sender()))
    );
    assert!(
        step.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == key.receiver()))
    );
    assert_eq!(
        restored.delivery_usage(key.sender()).unwrap().submitted,
        usage.unwrap().submitted
    );
}

#[test]
fn heartbeats_do_not_extend_publication_admission_or_settlement_deadlines() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let publication = fixture.begin_registration(&mut relay);
    let original = relay.deadline().unwrap();
    assert_ack(
        &probe(&mut relay, connection(1), 1, UnixMillis(original.0 - 1)),
        connection(1),
        1,
    );
    assert_eq!(relay.deadline(), Some(original));
    let expired = complete(&mut relay, &publication, original);
    assert!(
        expired
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == connection(1)))
    );

    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    for source in [key.sender(), key.receiver()] {
        let receipt = receive_from(&mut relay, key, source, fixture.at);
        assert_eq!(
            relay
                .submit(&receipt, fixture.at, |_| Submission::Complete)
                .refusal,
            None
        );
    }
    let original = relay.sessions.entries()[&key.wire_id()]
        .session
        .deadline()
        .unwrap();
    for peer in [key.sender(), key.receiver()] {
        assert_ack(
            &probe(&mut relay, peer, 1, UnixMillis(original.0 - 1)),
            peer,
            1,
        );
    }
    let expired = relay.handle(Input::Tick, original);
    assert!(
        expired
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Discard(_)))
    );
    for flow in flows {
        assert_eq!(relay.handle(Input::Discarded(flow), original).refusal, None);
    }
    let settlement = relay.deadline().unwrap();
    for peer in [key.sender(), key.receiver()] {
        assert_ack(
            &probe(&mut relay, peer, 2, UnixMillis(settlement.0 - 1)),
            peer,
            2,
        );
    }
    assert_eq!(relay.deadline(), Some(settlement));
    assert!(
        relay
            .handle(Input::Tick, settlement)
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(_)))
    );
}
