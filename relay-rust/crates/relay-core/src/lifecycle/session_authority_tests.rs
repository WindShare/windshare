use super::*;

#[test]
fn join_refusals_do_not_mint_a_session_or_extend_its_budget() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let input = Event::Join {
        request: request(&fixture, connection(2, 1), fixture.at),
        session: wire_id(1),
    };
    assert_eq!(
        engine.handle(input, fixture.at).action,
        Action::Refused(Refusal::NotFound)
    );
    fixture.register(&mut engine);
    let pending = request(&fixture, connection(2, 1), fixture.at);
    let deadline = pending.deadline();
    assert_eq!(
        engine
            .handle(
                Event::Join {
                    request: pending,
                    session: wire_id(1)
                },
                deadline
            )
            .action,
        Action::Refused(Refusal::JoinExpired)
    );
    let foreign = JoinRequest::new(
        RelayIdentity::new([99; 32]).unwrap(),
        connection(2, 1),
        fixture.share(),
        crate::flow::ReceiveCapacity::new(16, 16 * u64::from(OpaqueHeader::MAX_ENCODED_BYTES))
            .unwrap(),
        fixture.at,
    );
    assert_eq!(
        engine
            .handle(
                Event::Join {
                    request: foreign,
                    session: wire_id(1)
                },
                fixture.at
            )
            .action,
        Action::Refused(Refusal::WrongRelay)
    );
    engine.last_sequence = u64::MAX;
    let input = Event::Join {
        request: request(&fixture, connection(2, 1), fixture.at),
        session: wire_id(1),
    };
    assert_eq!(
        engine.handle(input, fixture.at).action,
        Action::Refused(Refusal::SequenceExhausted)
    );
}

#[test]
fn foreign_actor_and_role_confusion_never_grant_forwarding() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let permit = forward(&engine, &mut session, connection(2, 1), fixture.at);
    let (foreign, _) = ShareLifecycle::restore(
        RelayIdentity::new([99; 32]).unwrap(),
        fixture.share(),
        Restoration::restarted(None),
        Incarnation::new([3; 16]).unwrap(),
        Recorder::default(),
        fixture.at,
        |_, _| {},
    )
    .unwrap();
    assert_eq!(
        session
            .check_forward(&foreign, permit, fixture.at)
            .unwrap_err(),
        ForwardRefusal::ForeignLifecycle
    );
    assert_eq!(
        session
            .handle(&foreign, SessionEvent::Tick, fixture.at, &mut ())
            .action,
        SessionAction::Refused(sessions::Refusal::ForeignLifecycle)
    );
    assert!(session.check_forward(&engine, permit, fixture.at).is_ok());
    let wrong_sender = admitted(&session, session.key().receiver());
    assert_eq!(
        session
            .handle(&engine, wrong_sender, fixture.at, &mut ())
            .action,
        SessionAction::Refused(sessions::Refusal::WrongParticipant)
    );
    let other = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    let foreign_frame = frame(&other, session.key().receiver());
    assert_eq!(
        session
            .handle(&engine, foreign_frame, fixture.at, &mut ())
            .action,
        SessionAction::Refused(sessions::Refusal::StaleSession)
    );
    let foreign_admit = admitted(&other, session.key().sender());
    assert_eq!(
        session
            .handle(&engine, foreign_admit, fixture.at, &mut ())
            .action,
        SessionAction::Refused(sessions::Refusal::StaleSession)
    );
}
