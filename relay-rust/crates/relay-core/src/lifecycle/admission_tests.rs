use super::*;

#[test]
fn fresh_admission_is_bound_to_relay_connection_registration_and_deadline() {
    let fixture = Fixture::new();
    for mismatch in 0..5 {
        let mut engine = fixture.engine();
        let mut owner = connection(1, 1);
        let mut init_bytes = bytes(&fixture.vector.register_init_b64);
        let mut deadline = fixture.handshake(owner, fixture.at).deadline().unwrap();
        match mismatch {
            0 => engine.relay = RelayIdentity::new([3; 32]).unwrap(),
            1 => owner = connection(2, 1),
            2 => owner = connection(1, 2),
            3 => *init_bytes.last_mut().unwrap() ^= 1,
            _ => deadline = UnixMillis(deadline.0 + 1),
        }
        let Action::Reserved(reservation) = engine
            .handle(
                Event::Reserve {
                    connection: owner,
                    init: RegisterInit::decode(&init_bytes).unwrap(),
                    deadline,
                },
                fixture.at,
            )
            .action
        else {
            panic!("mismatched but valid reservation");
        };
        let id = reservation.id();
        let mut handshake = fixture.handshake(connection(1, 1), fixture.at);
        assert_eq!(
            handshake.admit(Admission::Fresh(reservation), fixture.seed(), fixture.at),
            Step::Close(CloseReason::AdmissionMismatch)
        );
        assert_eq!(handshake.phase(), crate::handshake::Phase::Finished);
        assert_eq!(
            engine
                .handle(
                    Event::Cancel {
                        id,
                        reason: Cancellation::HandshakeFailed
                    },
                    fixture.at
                )
                .action,
            Action::Released(id)
        );
    }
}

#[test]
fn resume_admission_is_bound_to_relay_connection_registration_and_deadline() {
    let fixture = Fixture::new();
    let at = fixture.resume_at;
    for mismatch in 0..6 {
        let mut engine = fixture.engine();
        let mut event = fixture.resume_event(connection(1, 1));
        let Event::BeginResume {
            connection: owner,
            init,
            deadline,
        } = &mut event
        else {
            unreachable!();
        };
        match mismatch {
            0 => engine.relay = RelayIdentity::new([3; 32]).unwrap(),
            1 => *owner = connection(2, 1),
            2 => *owner = connection(1, 2),
            3 => *init = fixture.foreign_resume_init(),
            4 => *deadline = UnixMillis(deadline.0 + 1),
            _ => {}
        }
        let admission = if mismatch == 5 {
            // A resume handshake never spends a challenge on a fresh reservation.
            Admission::Fresh(fixture.reserve(&mut engine, connection(1, 1), fixture.at))
        } else {
            let Action::ResumeAdmitted(attempt) = engine.handle(event, at).action else {
                panic!("mismatched but valid resume admission");
            };
            Admission::Resume(attempt)
        };
        let mut handshake = ControlHandshake::open(fixture.relay(), connection(1, 1), at);
        assert_eq!(
            handshake.receive(&bytes(&fixture.vector.resume_init_b64), at),
            Step::AwaitFrame
        );
        assert!(matches!(
            handshake.receive(&bytes(&fixture.vector.resume_credential_b64), at),
            Step::RequestAdmission(_)
        ));
        assert_eq!(
            handshake.admit(admission, fixture.seed(), at),
            Step::Close(CloseReason::AdmissionMismatch)
        );
        assert_eq!(handshake.phase(), crate::handshake::Phase::Finished);
    }
}

#[test]
fn fresh_handshake_cannot_spend_a_challenge_without_its_reservation() {
    let fixture = Fixture::new();
    let attempt = fixture.begin_resume(&mut fixture.engine(), connection(1, 1));
    for admission in [Admission::Resume(attempt), Admission::Stop] {
        let mut handshake = fixture.handshake(connection(1, 1), fixture.at);
        assert_eq!(
            handshake.admit(admission, fixture.seed(), fixture.at),
            Step::Close(CloseReason::AdmissionMismatch)
        );
        assert_eq!(
            handshake.receive(&bytes(&fixture.vector.register_proof_b64), fixture.at),
            Step::Close(CloseReason::UnexpectedFrame)
        );
    }
}

#[test]
fn delayed_admission_does_not_extend_either_handshake_or_route_ownership() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let reservation = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
    let deadline = reservation.deadline();
    let id = reservation.id();
    let mut handshake = fixture.handshake(connection(1, 1), fixture.at);
    assert_eq!(
        handshake.admit(Admission::Fresh(reservation), fixture.seed(), deadline),
        Step::Close(CloseReason::Expired)
    );
    let effects = engine.handle(
        Event::Cancel {
            id,
            reason: Cancellation::HandshakeFailed,
        },
        deadline,
    );
    assert_eq!(effects.expiration, Some(Expiration::Released(id)));
    assert_eq!(effects.action, Action::Refused(Refusal::StaleOperation));
    assert_eq!(engine.phase(), Phase::Vacant);
}

#[test]
fn wrong_route_resume_and_expired_requests_do_not_reserve_capacity() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let resume = RegisterInit::decode(&bytes(&fixture.vector.resume_init_b64)).unwrap();
    let deadline = fixture.at.saturating_add(Duration::from_secs(1));
    assert_eq!(
        engine
            .handle(
                Event::Reserve {
                    connection: connection(1, 1),
                    init: resume,
                    deadline
                },
                fixture.at
            )
            .action,
        Action::Refused(Refusal::UnsupportedMode)
    );
    assert_eq!(
        engine
            .handle(
                fixture.reserve_event(fixture.at),
                deadline.saturating_add(Duration::from_secs(10))
            )
            .action,
        Action::Refused(Refusal::Expired)
    );
    assert_eq!(engine.phase(), Phase::Vacant);
    assert_eq!(engine.last_sequence, 0);

    let other_share = ShareId::new([0x5a; 12]).unwrap();
    assert_ne!(other_share, fixture.share());
    assert_eq!(
        engine.handle(Event::Join(other_share), fixture.at).action,
        Action::Refused(Refusal::WrongShare)
    );
    let (mut wrong_actor, _) = ShareLifecycle::restore(
        fixture.relay(),
        other_share,
        None,
        [],
        Incarnation::new([1; 16]).unwrap(),
        Recorder::default(),
        fixture.at,
    )
    .unwrap();
    assert_eq!(
        wrong_actor
            .handle(fixture.reserve_event(fixture.at), fixture.at)
            .action,
        Action::Refused(Refusal::WrongShare)
    );
    assert_eq!(wrong_actor.phase(), Phase::Vacant);
}

#[test]
fn attempt_sequences_never_wrap_into_an_old_identity() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    engine.last_sequence = u64::MAX - 1;
    let reservation = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
    assert_eq!(reservation.id().sequence().get(), u64::MAX);
    assert_eq!(reservation.id().relay(), fixture.relay());
    assert_eq!(reservation.id().share(), fixture.share());
    let id = reservation.id();
    assert_eq!(
        engine
            .handle(
                Event::Cancel {
                    id,
                    reason: Cancellation::HostFailed
                },
                fixture.at
            )
            .action,
        Action::Released(id)
    );
    assert_eq!(
        engine
            .handle(fixture.reserve_event(fixture.at), fixture.at)
            .action,
        Action::Refused(Refusal::SequenceExhausted)
    );
    assert_eq!(engine.phase(), Phase::Vacant);
    assert_eq!(engine.last_sequence, u64::MAX);
    assert!(Incarnation::new([0; 16]).is_none());
}

#[test]
fn only_peer_refusals_have_wire_responses() {
    use crate::error_frame::{ErrorFrame, RetryAfter};

    let retry = RetryAfter::from_millis(250).unwrap();
    let cases = [
        (Refusal::WrongShare, Some(ErrorFrame::NotFound)),
        (Refusal::NotFound, Some(ErrorFrame::NotFound)),
        (Refusal::UnsupportedMode, Some(ErrorFrame::UnsupportedMode)),
        (Refusal::Expired, Some(ErrorFrame::ChallengeExpired)),
        (Refusal::Occupied, Some(ErrorFrame::AlreadyRegistered)),
        (
            Refusal::PublicationUncertain,
            Some(ErrorFrame::Admission(retry)),
        ),
        (
            Refusal::SequenceExhausted,
            Some(ErrorFrame::Admission(retry)),
        ),
        (Refusal::Starting, Some(ErrorFrame::Starting(retry))),
        (Refusal::SenderDetached, Some(ErrorFrame::Starting(retry))),
        (Refusal::Stopped, Some(ErrorFrame::Stopped)),
        (Refusal::ResumeMismatch, Some(ErrorFrame::InvalidProof)),
        (Refusal::ResumeStale, Some(ErrorFrame::ResumeStale)),
        (Refusal::StaleOperation, None),
    ];
    for (refusal, frame) in cases {
        assert_eq!(refusal.frame(), frame);
    }
}
