use super::*;

#[test]
fn stale_feedback_and_cleanup_cannot_touch_a_reused_slot_or_sibling() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut old = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let sibling = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    let mut ledger = ledger(2, &fixture, old.key().sender());
    let stale = open(&mut ledger, &engine, &old, Direction::ToSender, fixture.at);
    let sibling_flow = open(
        &mut ledger,
        &engine,
        &sibling,
        Direction::ToSender,
        fixture.at,
    );
    let receipt = receive(&mut ledger, &engine, &mut old, stale, 1, fixture.at);
    submit(&mut ledger, &engine, &old, &receipt, fixture.at);
    let retirement = cancel(&engine, &mut old, fixture.at);
    assert_eq!(ledger.retire(retirement, fixture.at), Ok(Some(stale)));
    let notice = ledger.discarded(stale, fixture.at).unwrap().unwrap();
    assert_eq!(
        ledger.settle(stale, stale.destination(), notice.submitted, fixture.at),
        Ok(Feedback::Settled(notice.submitted))
    );
    let replacement = join(&fixture, &mut engine, connection(2, 2), 3, fixture.at);
    let fresh = open(
        &mut ledger,
        &engine,
        &replacement,
        Direction::ToSender,
        fixture.at,
    );
    let before = ledger.usage();
    assert_ne!(stale, fresh);
    assert!(ledger.snapshot(stale).is_none());
    assert_eq!(
        ledger.consume(stale, stale.destination(), notice.submitted, fixture.at),
        Ok(Feedback::Stale)
    );
    assert_eq!(
        ledger.settle(stale, stale.destination(), notice.submitted, fixture.at),
        Ok(Feedback::Stale)
    );
    assert_eq!(ledger.discarded(stale, fixture.at), Ok(None));
    assert_eq!(ledger.retire(retirement, fixture.at), Ok(None));
    assert_eq!(ledger.usage(), before);
    assert_eq!(
        ledger.snapshot(sibling_flow).unwrap().usage.total(),
        counts(1)
    );
    assert_eq!(ledger.snapshot(fresh).unwrap().usage.total(), counts(1));
    let route = replacement
        .check_forward(&engine, fresh.permit(), fixture.at)
        .unwrap();
    assert_eq!(
        ledger.submit(&route, &receipt, fixture.at),
        Err(FlowRefusal::WrongRoute)
    );
}

#[test]
fn routes_headers_generations_and_feedback_directions_are_checked_before_mutation() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let first = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let second = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    let mut sender = ledger(3, &fixture, first.key().sender());
    let mut receiver = ledger(2, &fixture, first.key().receiver());
    let flow = open(
        &mut sender,
        &engine,
        &first,
        Direction::ToSender,
        fixture.at,
    );
    let down = open(
        &mut receiver,
        &engine,
        &first,
        Direction::ToReceiver,
        fixture.at,
    );
    let route = first
        .check_forward(&engine, flow.permit(), fixture.at)
        .unwrap();
    let other = second
        .check_route(&engine, Direction::ToSender, fixture.at)
        .unwrap();
    let before = sender.usage();
    assert_eq!(
        sender.open(&route, fixture.at),
        Err(FlowRefusal::AlreadyOpen)
    );
    assert_eq!(
        receiver.open(&route, fixture.at),
        Err(FlowRefusal::WrongDestination)
    );
    assert_eq!(
        sender.grant(flow, &other, counts(1), fixture.at),
        Err(FlowRefusal::WrongRoute)
    );
    assert_eq!(
        sender.receive(flow, &route, header(&second, 1), fixture.at),
        Err(FlowRefusal::WrongSession)
    );
    assert_eq!(
        sender.consume(flow, first.key().receiver(), counts(1), fixture.at),
        Err(FlowRefusal::WrongDestination)
    );
    assert_eq!(
        sender.consume(flow, connection(1, 2), counts(1), fixture.at),
        Err(FlowRefusal::WrongDestination)
    );
    assert_eq!(
        sender.consume(down, flow.destination(), counts(1), fixture.at),
        Err(FlowRefusal::WrongDestination)
    );
    assert_eq!(
        receiver.settle(flow, down.destination(), counts(1), fixture.at),
        Err(FlowRefusal::WrongDestination)
    );
    assert_eq!(sender.usage(), before);
    let observation = sender.observer().0.last().unwrap();
    assert_eq!(observation.destination, first.key().sender());
    assert_eq!(
        observation.input,
        flow::Input::Consume {
            flow: down,
            from: flow.destination(),
            total: counts(1)
        }
    );
    assert_eq!(
        observation.decision,
        flow::Decision::Refused(FlowRefusal::WrongDestination)
    );
}

#[test]
fn stop_fences_submission_before_durable_cleanup_and_keeps_submitted_occupancy() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let mut ledger = ledger(1, &fixture, session.key().sender());
    let flow = open(
        &mut ledger,
        &engine,
        &session,
        Direction::ToSender,
        fixture.at,
    );
    {
        let route = session
            .check_forward(&engine, flow.permit(), fixture.at)
            .unwrap();
        let grant = ledger.grant(flow, &route, counts(1), fixture.at).unwrap();
        assert_eq!(grant.credit, counts(1));
    }
    let sent = receive(&mut ledger, &engine, &mut session, flow, 1, fixture.at);
    let queued = receive(&mut ledger, &engine, &mut session, flow, 1, fixture.at);
    submit(&mut ledger, &engine, &session, &sent, fixture.at);
    let authority = stopping::authority(&fixture, connection(4, 1));
    let effects = engine.handle(
        Event::Stop {
            connection: connection(4, 1),
            authority,
        },
        fixture.at,
    );
    assert!(matches!(effects.action, Action::Stop(StopWork::Persist(_))));
    assert_eq!(
        session
            .check_forward(&engine, queued.flow().permit(), fixture.at)
            .unwrap_err(),
        ForwardRefusal::Retired(RetirementReason::Stopped)
    );
    let revoked = session.handle(
        &engine,
        SessionEvent::Revoke(effects.revocation.unwrap()),
        fixture.at,
        &mut (),
    );
    assert_eq!(
        ledger.retire(revoked.retirement.unwrap(), fixture.at),
        Ok(Some(flow))
    );
    assert_eq!(ledger.usage().received, Counts::new(1, SMALL_FRAME));
    assert_eq!(ledger.usage().submitted, Counts::new(1, SMALL_FRAME));
    let notice = ledger.discarded(flow, fixture.at).unwrap().unwrap();
    assert_eq!(notice.submitted, Counts::new(1, SMALL_FRAME));
    assert_eq!(ledger.usage().submitted, Counts::new(1, SMALL_FRAME));
}

#[test]
fn resume_separates_old_and_new_destination_ledgers_until_old_closure_is_confirmed() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut old = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    activate(&engine, &mut old, fixture.at);
    let mut old_ledger = ledger(1, &fixture, old.key().sender());
    let stale = open(
        &mut old_ledger,
        &engine,
        &old,
        Direction::ToSender,
        fixture.at,
    );
    let receipt = receive(&mut old_ledger, &engine, &mut old, stale, 1, fixture.at);
    submit(&mut old_ledger, &engine, &old, &receipt, fixture.at);
    let effects = fixture.resume(&mut engine, connection(1, 2));
    assert!(
        old.check_forward(&engine, stale.permit(), fixture.resume_at)
            .is_err()
    );
    let revoked = old.handle(
        &engine,
        SessionEvent::Revoke(effects.revocation.unwrap()),
        fixture.resume_at,
        &mut (),
    );
    assert_eq!(
        old_ledger.retire(revoked.retirement.unwrap(), fixture.resume_at),
        Ok(Some(stale))
    );
    let commit = ownership_commit(effects);
    committed_registration(&mut engine, commit.id(), fixture.resume_at);
    let fresh = join(
        &fixture,
        &mut engine,
        connection(2, 2),
        2,
        fixture.resume_at,
    );
    let mut new_ledger = ledger(1, &fixture, fresh.key().sender());
    let new_flow = open(
        &mut new_ledger,
        &engine,
        &fresh,
        Direction::ToSender,
        fixture.resume_at,
    );
    assert_eq!(
        new_ledger.consume(
            stale,
            stale.destination(),
            Counts::new(1, SMALL_FRAME),
            fixture.resume_at
        ),
        Err(FlowRefusal::WrongDestination)
    );
    assert_eq!(
        new_ledger.confirm_closed(stale.destination(), fixture.resume_at),
        Err(FlowRefusal::WrongDestination)
    );
    assert_eq!(
        old_ledger.consume(
            stale,
            fresh.key().sender(),
            Counts::new(1, SMALL_FRAME),
            fixture.resume_at
        ),
        Err(FlowRefusal::WrongDestination)
    );
    assert_eq!(
        old_ledger.confirm_closed(stale.destination(), fixture.resume_at),
        Ok(Counts::new(1, SMALL_FRAME))
    );
    assert_eq!(
        new_ledger.snapshot(new_flow).unwrap().usage.total(),
        counts(1)
    );
}

#[test]
fn expired_provisional_routes_cannot_issue_startup_credit_or_submit_queued_frames() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let mut ledger = ledger(1, &fixture, session.key().sender());
    let flow = open(
        &mut ledger,
        &engine,
        &session,
        Direction::ToSender,
        fixture.at,
    );
    let queued = receive(&mut ledger, &engine, &mut session, flow, 1, fixture.at);
    let deadline = session.deadline().unwrap();
    assert_eq!(
        session
            .check_route(&engine, Direction::ToReceiver, deadline)
            .unwrap_err(),
        ForwardRefusal::Retired(RetirementReason::AdmissionExpired)
    );
    assert!(
        session
            .check_forward(&engine, queued.flow().permit(), deadline)
            .is_err()
    );
    let expiry = session
        .handle(&engine, SessionEvent::Tick, deadline, &mut ())
        .retirement
        .unwrap();
    assert_eq!(ledger.retire(expiry, deadline), Ok(Some(flow)));
    assert_eq!(ledger.usage().received, Counts::new(1, SMALL_FRAME));
    assert_eq!(
        ledger.discarded(flow, deadline).unwrap().unwrap().submitted,
        Counts::ZERO
    );
    assert_eq!(ledger.usage().total(), Counts::ZERO);
}
