use super::*;

#[test]
fn join_cannot_cross_stop_or_create_authority_on_the_sender_connection() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let input = Event::Join {
        request: request(&fixture, published.owner.connection(), fixture.at),
        session: wire_id(1),
    };
    assert_eq!(
        engine.handle(input, fixture.at).action,
        Action::Refused(Refusal::InvalidReceiver)
    );
    assert_eq!(
        Refusal::InvalidReceiver.frame(),
        Some(crate::error_frame::ErrorFrame::Malformed)
    );
    let pending = request(&fixture, connection(2, 1), fixture.at);
    let mut joined = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    let permit = forward(&engine, &mut joined, connection(3, 1), fixture.at);
    let authority = stopping::authority(&fixture, connection(4, 1));
    let effects = engine.handle(
        Event::Stop {
            connection: connection(4, 1),
            authority,
        },
        fixture.at,
    );
    assert!(matches!(effects.action, Action::Stop(StopWork::Persist(_))));
    let revoke = effects.revocation.unwrap();
    assert_eq!(revoke.owner(), published.owner);
    assert_eq!(revoke.reason(), RetirementReason::Stopped);
    // Neither delayed cleanup nor uncertain persistence leaves the queued permit live.
    assert_eq!(
        joined
            .check_forward(&engine, permit, fixture.at)
            .unwrap_err(),
        ForwardRefusal::Retired(RetirementReason::Stopped)
    );
    let input = Event::Join {
        request: pending,
        session: wire_id(3),
    };
    assert_eq!(
        engine.handle(input, fixture.at).action,
        Action::Refused(Refusal::StopPending)
    );
    let retirement = joined.handle(&engine, SessionEvent::Revoke(revoke), fixture.at, &mut ());
    assert_eq!(
        retirement.retirement.unwrap().reason,
        RetirementReason::Stopped
    );
    let late = admitted(&joined, published.owner.connection());
    assert_eq!(
        joined.handle(&engine, late, fixture.at, &mut ()).action,
        SessionAction::Discard(RetirementReason::Stopped)
    );
}

#[test]
fn resume_revokes_old_permits_before_cleanup_and_late_callbacks_spare_the_successor() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut old = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    activate(&engine, &mut old, fixture.at);
    let permit = forward(&engine, &mut old, connection(2, 1), fixture.at);
    let replacement = connection(1, 2);
    let effects = fixture.resume(&mut engine, replacement);
    let revoke = effects.revocation.unwrap();
    assert_eq!(revoke.owner(), old.key().owner());
    assert_eq!(revoke.reason(), RetirementReason::OwnerReplaced);
    assert!(
        old.check_forward(&engine, permit, fixture.resume_at)
            .is_err()
    );
    let mut fresh = join(
        &fixture,
        &mut engine,
        connection(2, 2),
        2,
        fixture.resume_at,
    );
    let current = forward(&engine, &mut fresh, connection(2, 2), fixture.resume_at);
    assert_eq!(
        fresh
            .check_forward(&engine, permit, fixture.resume_at)
            .unwrap_err(),
        ForwardRefusal::StaleSession
    );
    let stale_cleanup = fresh.handle(
        &engine,
        SessionEvent::Revoke(revoke),
        fixture.resume_at,
        &mut (),
    );
    assert_eq!(stale_cleanup.retirement, None);
    assert_eq!(
        stale_cleanup.action,
        SessionAction::Refused(sessions::Refusal::StaleRevocation)
    );
    let stale_failure = SessionEvent::End {
        session: old.key(),
        participant: old.key().receiver(),
        reason: EndReason::HostFailed,
    };
    let mut trace = SessionTrace::default();
    assert_eq!(
        fresh
            .handle(&engine, stale_failure, fixture.resume_at, &mut trace)
            .action,
        SessionAction::Refused(sessions::Refusal::StaleSession)
    );
    assert_eq!(trace.0[0].session, fresh.key());
    assert_eq!(trace.0[0].event, stale_failure);
    let closed_old = engine.handle(
        Event::OwnerDisconnected(old.key().sender()),
        fixture.resume_at,
    );
    assert_eq!(closed_old.revocation, None);
    assert!(
        fresh
            .check_forward(&engine, current, fixture.resume_at)
            .is_ok()
    );
    assert_eq!(
        old.handle(
            &engine,
            SessionEvent::Revoke(revoke),
            fixture.resume_at,
            &mut ()
        )
        .retirement
        .unwrap()
        .session,
        old.key()
    );
}

#[test]
fn a_receiver_disconnect_revokes_only_its_session_and_rejects_reused_slot_callbacks() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut first = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let mut sibling = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    activate(&engine, &mut first, fixture.at);
    activate(&engine, &mut sibling, fixture.at);
    let sender = first.key().sender();
    let first_permit = forward(&engine, &mut first, sender, fixture.at);
    let sibling_permit = forward(&engine, &mut sibling, sender, fixture.at);
    for participant in [connection(2, 2), sender] {
        let wrong_participant = SessionEvent::End {
            session: first.key(),
            participant,
            reason: EndReason::ReceiverDisconnected,
        };
        assert_eq!(
            first
                .handle(&engine, wrong_participant, fixture.at, &mut ())
                .action,
            SessionAction::Refused(sessions::Refusal::WrongParticipant)
        );
    }
    let ended = SessionEvent::End {
        session: first.key(),
        participant: connection(2, 1),
        reason: EndReason::ReceiverDisconnected,
    };
    let cleanup = first.handle(&engine, ended, fixture.at, &mut ());
    assert_eq!(cleanup.retirement.unwrap().session, first.key());
    assert!(
        first
            .check_forward(&engine, first_permit, fixture.at)
            .is_err()
    );
    assert!(
        sibling
            .check_forward(&engine, sibling_permit, fixture.at)
            .is_ok()
    );
    assert_eq!(
        first.handle(&engine, ended, fixture.at, &mut ()).retirement,
        None
    );
    let late = frame(&first, sender);
    assert!(matches!(
        first.handle(&engine, late, fixture.at, &mut ()).action,
        SessionAction::Discard(_)
    ));
    let forged = frame(&first, connection(9, 1));
    assert_eq!(
        first.handle(&engine, forged, fixture.at, &mut ()).action,
        SessionAction::Refused(sessions::Refusal::WrongParticipant)
    );
}

#[test]
fn losing_the_owner_revokes_every_old_session_without_waiting_for_grace() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let mut first = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let mut second = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    activate(&engine, &mut second, fixture.at);
    let permit = forward(
        &engine,
        &mut first,
        published.owner.connection(),
        fixture.at,
    );
    let effects = engine.handle(
        Event::OwnerDisconnected(published.owner.connection()),
        fixture.at,
    );
    let revoke = effects.revocation.unwrap();
    assert_eq!(revoke.reason(), RetirementReason::OwnerDisconnected);
    assert!(first.check_forward(&engine, permit, fixture.at).is_err());
    let result = first.handle(&engine, SessionEvent::Revoke(revoke), fixture.at, &mut ());
    assert_eq!(
        result.retirement.unwrap().reason,
        RetirementReason::OwnerDisconnected
    );
    assert_eq!(engine.phase(), Phase::Grace);
    let resumed = fixture.resume(&mut engine, connection(1, 2));
    assert!(matches!(resumed.action, Action::Resumed(_)));
    // A delayed disconnect cleanup keeps its original cause after a resume.
    let delayed = second.handle(
        &engine,
        SessionEvent::Revoke(revoke),
        fixture.resume_at,
        &mut (),
    );
    assert_eq!(
        delayed.retirement.unwrap().reason,
        RetirementReason::OwnerDisconnected
    );
    assert_eq!(
        second
            .handle(
                &engine,
                SessionEvent::Revoke(revoke),
                fixture.resume_at,
                &mut ()
            )
            .retirement,
        None
    );
}
