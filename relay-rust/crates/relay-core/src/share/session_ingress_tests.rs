use super::*;

fn header(session: RelaySessionId) -> OpaqueHeader {
    let mut writer = Writer::<21>::with_header(*b"WS2O", 0);
    writer
        .put(session.as_bytes())
        .put(&1_u32.to_be_bytes())
        .put(&[1]);
    OpaqueHeader::decode(&writer.into_frame()).unwrap()
}

fn admission(session: RelaySessionId) -> Vec<u8> {
    let mut writer = Writer::<16>::with_header(*b"WS2M", 0);
    writer.put(session.as_bytes());
    writer.into_frame().to_vec()
}

fn sender_liveness(relay: &Relay, sender: ConnectionRef) -> (UnixMillis, UnixMillis) {
    let Some(liveness) = relay.generations.connection(&sender).unwrap().liveness() else {
        panic!("the shared sender must remain established");
    };
    (liveness.last_inbound(), liveness.deadline())
}

fn late_inputs(relay: &mut Relay, key: SessionKey, at: UnixMillis) {
    let usage = relay.delivery_usage(key.sender());
    let liveness = sender_liveness(relay, key.sender());
    let admitted = admission(key.wire_id());
    for input in [
        Input::Frame {
            source: key.sender(),
            header: header(key.wire_id()),
        },
        Input::Control {
            connection: key.sender(),
            frame: &admitted,
        },
        Input::Cancel {
            participant: key.sender(),
            session: key.wire_id(),
        },
    ] {
        let step = relay.handle(input, at);
        assert_eq!(step.refusal, None);
        assert!(step.outputs.is_empty(), "{step:?}");
        assert_eq!(relay.delivery_usage(key.sender()), usage);
        assert_eq!(
            sender_liveness(relay, key.sender()),
            liveness,
            "discarded traffic must not refresh liveness"
        );
    }
}

#[test]
fn receiver_loss_discards_late_ingress_before_and_after_session_collection() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (retired, flows) = fixture.join(&mut relay, 2);
    let (healthy, _) = fixture.join(&mut relay, 3);
    let pending = receive(&mut relay, retired, fixture.at);
    let _ = relay.handle(Input::Disconnected(retired.receiver()), fixture.at);
    assert!(relay.sessions.entries().contains_key(&retired.wire_id()));
    let later = UnixMillis(fixture.at.0 + 1);
    late_inputs(&mut relay, retired, later);
    assert!(
        relay
            .submit(&pending, later, |_| panic!("revoked handoff"))
            .refusal
            .is_some()
    );
    for flow in flows {
        let step = relay.handle(Input::Discarded(flow), later);
        assert_eq!(step.refusal, None);
    }
    let _ = relay.handle(Input::Closed(retired.receiver()), later);
    assert!(!relay.sessions.entries().contains_key(&retired.wire_id()));
    late_inputs(&mut relay, retired, UnixMillis(later.0 + 1));
    assert!(relay.observer().0.iter().any(|observation| matches!(
        observation,
        Observation::Relay {
            connection: Some(source),
            decision: RelayDecision::CollectedSessionInput {
                session, kind: crate::observation::SessionInputKind::Frame,
            }, ..
        } if *source == retired.sender() && *session == retired.wire_id()
    )));
    for source in [healthy.sender(), healthy.receiver()] {
        let received = receive_from(&mut relay, healthy, source, later);
        let step = relay.submit(&received, later, |_| Submission::Complete);
        assert_eq!(step.refusal, None);
    }
}

#[test]
fn expired_admission_discards_late_sender_confirmation_and_preserves_healthy_session() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (expired, _) = fixture.join(&mut relay, 2);
    let (healthy, _) = fixture.join(&mut relay, 3);
    let _ = receive(&mut relay, healthy, fixture.at);
    activate(&mut relay, healthy, fixture.at);
    let expiry = relay.sessions.entries()[&expired.wire_id()]
        .session
        .deadline()
        .unwrap();
    let step = relay.handle(
        Input::Frame {
            source: expired.sender(),
            header: header(expired.wire_id()),
        },
        expiry,
    );
    assert_eq!(step.refusal, None);
    assert!(
        step.outputs
            .iter()
            .any(|output| matches!(output, Output::Discard(_)))
    );
    assert!(
        !step
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Received(_) | Output::Close(_)))
    );
    late_inputs(&mut relay, expired, expiry);
    let _ = receive_from(&mut relay, healthy, healthy.sender(), expiry);
}

#[test]
fn foreign_participants_and_unissued_session_ids_remain_refused() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    let foreign = connection(3);
    let _ = relay.handle(Input::Connected(foreign), fixture.at);
    for retired in [false, true] {
        if retired {
            let _ = relay.handle(Input::Disconnected(key.receiver()), fixture.at);
        }
        let admitted = admission(key.wire_id());
        for input in [
            Input::Frame {
                source: foreign,
                header: header(key.wire_id()),
            },
            Input::Admitted {
                sender: foreign,
                frame: SessionAdmitted::decode(&admitted).unwrap(),
            },
            Input::Cancel {
                participant: foreign,
                session: key.wire_id(),
            },
        ] {
            let step = relay.handle(input, fixture.at);
            assert_eq!(
                step.refusal,
                Some(Error::Session(crate::sessions::Refusal::WrongParticipant))
            );
            assert!(step.outputs.is_empty());
        }
    }
    let unissued = RelaySessionId::new(u64::MAX.to_be_bytes()).unwrap();
    let admitted = admission(unissued);
    for input in [
        Input::Frame {
            source: key.sender(),
            header: header(unissued),
        },
        Input::Control {
            connection: key.sender(),
            frame: &admitted,
        },
        Input::Cancel {
            participant: key.sender(),
            session: unissued,
        },
    ] {
        let step = relay.handle(input, fixture.at);
        assert_eq!(step.refusal, Some(Error::UnknownSession));
        assert!(step.outputs.is_empty());
    }
}
