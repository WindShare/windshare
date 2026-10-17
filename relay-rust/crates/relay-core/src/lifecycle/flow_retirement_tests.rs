use super::*;

#[test]
fn cancellation_retains_payloads_until_queue_discard_and_final_settlement() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let mut ledger = ledger::<2>(&fixture, session.key().sender());
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
        let grant = ledger.grant(flow, &route, counts(3), fixture.at).unwrap();
        assert_eq!(grant.credit, counts(3));
    }
    let first = receive(&mut ledger, &engine, &mut session, flow, 1, fixture.at);
    let second = receive(&mut ledger, &engine, &mut session, flow, 2, fixture.at);
    let queued = receive(&mut ledger, &engine, &mut session, flow, 3, fixture.at);
    submit(&mut ledger, &engine, &session, &first, fixture.at);
    submit(&mut ledger, &engine, &session, &second, fixture.at);
    let submitted = Counts::new(2, SMALL_FRAME * 2 + 1);
    let queued_cost = Counts::new(1, queued.encoded_bytes());
    let retirement = cancel(&engine, &mut session, fixture.at);
    assert!(
        session
            .check_forward(&engine, flow.permit(), fixture.at)
            .is_err()
    );
    assert_eq!(ledger.retire(retirement, fixture.at), Ok(Some(flow)));
    assert_eq!(
        ledger.usage(),
        Usage {
            granted: Counts::ZERO,
            received: queued_cost,
            submitted
        }
    );
    assert_eq!(
        ledger.snapshot(flow).unwrap().phase,
        flow::Phase::Discarding
    );
    assert_eq!(
        ledger.settle(flow, flow.destination(), submitted, fixture.at),
        Err(FlowRefusal::CleanupPending)
    );
    let first_total = Counts::new(1, first.encoded_bytes());
    assert_eq!(
        ledger.consume(flow, flow.destination(), first_total, fixture.at),
        Ok(Feedback::Released(first_total))
    );
    assert_eq!(ledger.usage().received, queued_cost);
    // This is the simulated host dropping its last queued payload before reporting cleanup.
    drop(queued);
    let notice = ledger.discarded(flow, fixture.at).unwrap().unwrap();
    assert_eq!(notice.submitted, submitted);
    assert_eq!(ledger.usage().received, Counts::ZERO);
    let remaining = Counts::new(1, second.encoded_bytes());
    assert_eq!(ledger.usage().submitted, remaining);
    assert_eq!(
        ledger.settle(flow, flow.destination(), first_total, fixture.at),
        Err(FlowRefusal::InvalidSettlement)
    );
    assert_eq!(
        ledger.settle(flow, flow.destination(), submitted, fixture.at),
        Ok(Feedback::Settled(remaining))
    );
    assert_eq!(ledger.usage(), Usage::default());
    assert_eq!(ledger.startup_used(), Counts::ZERO);
    assert_eq!(ledger.deadline(), None);
    assert_eq!(
        ledger.settle(flow, flow.destination(), submitted, fixture.at),
        Ok(Feedback::Stale)
    );
    assert_eq!(
        ledger.consume(flow, flow.destination(), submitted, fixture.at),
        Ok(Feedback::Stale)
    );
    assert_eq!(ledger.discarded(flow, fixture.at), Ok(None));
    assert_eq!(ledger.retire(retirement, fixture.at), Ok(None));
}

#[test]
fn consumption_can_finish_retirement_without_waiting_for_a_redundant_final_ack() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    for consume_before_cleanup in [true, false] {
        let receiver = connection(if consume_before_cleanup { 2 } else { 3 }, 1);
        let id = receiver.id.get();
        let mut session = join(&fixture, &mut engine, receiver, id, fixture.at);
        let mut ledger = ledger::<1>(&fixture, session.key().sender());
        let flow = open(
            &mut ledger,
            &engine,
            &session,
            Direction::ToSender,
            fixture.at,
        );
        let receipt = receive(&mut ledger, &engine, &mut session, flow, 1, fixture.at);
        submit(&mut ledger, &engine, &session, &receipt, fixture.at);
        let retirement = cancel(&engine, &mut session, fixture.at);
        assert_eq!(ledger.retire(retirement, fixture.at), Ok(Some(flow)));
        let total = Counts::new(1, SMALL_FRAME);
        if consume_before_cleanup {
            assert_eq!(
                ledger.consume(flow, flow.destination(), total, fixture.at),
                Ok(Feedback::Released(total))
            );
            assert!(ledger.snapshot(flow).is_some());
        }
        let notice = ledger.discarded(flow, fixture.at).unwrap().unwrap();
        assert_eq!(notice.submitted, total);
        if !consume_before_cleanup {
            assert_eq!(
                ledger.consume(flow, flow.destination(), total, fixture.at),
                Ok(Feedback::Settled(total))
            );
        }
        assert!(ledger.snapshot(flow).is_none());
        assert_eq!(ledger.deadline(), None);
        assert_eq!(
            ledger.tick(fixture.at.saturating_add(SETTLEMENT_TIMEOUT)),
            None
        );
    }
}

#[test]
fn retirement_before_any_submission_releases_capacity_only_after_cleanup_confirmation() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let mut ledger = ledger::<1>(&fixture, session.key().sender());
    let flow = open(
        &mut ledger,
        &engine,
        &session,
        Direction::ToSender,
        fixture.at,
    );
    assert_eq!(
        ledger.discarded(flow, fixture.at),
        Err(FlowRefusal::NotRetired)
    );
    assert_eq!(
        ledger.settle(flow, flow.destination(), Counts::ZERO, fixture.at),
        Err(FlowRefusal::NotRetired)
    );
    let queued = receive(&mut ledger, &engine, &mut session, flow, 1, fixture.at);
    let retirement = cancel(&engine, &mut session, fixture.at);
    assert_eq!(ledger.retire(retirement, fixture.at), Ok(Some(flow)));
    assert_eq!(ledger.usage().total(), Counts::new(1, SMALL_FRAME));
    drop(queued);
    assert_eq!(
        ledger
            .discarded(flow, fixture.at)
            .unwrap()
            .unwrap()
            .submitted,
        Counts::ZERO
    );
    assert_eq!(ledger.usage().total(), Counts::ZERO);
    assert!(ledger.snapshot(flow).is_none());
}

#[test]
fn stalled_cleanup_requests_closure_without_refunding_and_retries_do_not_extend_it() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let sibling = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    let mut ledger = ledger::<3>(&fixture, session.key().sender());
    let flow = open(
        &mut ledger,
        &engine,
        &session,
        Direction::ToSender,
        fixture.at,
    );
    let sibling_flow = open(
        &mut ledger,
        &engine,
        &sibling,
        Direction::ToSender,
        fixture.at,
    );
    let queued = receive(&mut ledger, &engine, &mut session, flow, 1, fixture.at);
    let retirement = cancel(&engine, &mut session, fixture.at);
    assert_eq!(ledger.retire(retirement, fixture.at), Ok(Some(flow)));
    let deadline = ledger.deadline().unwrap();
    let before_deadline = UnixMillis(deadline.0 - 1);
    assert_eq!(ledger.retire(retirement, before_deadline), Ok(Some(flow)));
    assert_eq!(ledger.deadline(), Some(deadline));
    assert_eq!(ledger.tick(before_deadline), None);
    let usage = ledger.usage();
    // Event-entry expiration must fence new work even if the timer was delayed.
    let route = sibling
        .check_forward(&engine, sibling_flow.permit(), deadline)
        .unwrap();
    assert_eq!(
        ledger.grant(sibling_flow, &route, counts(1), deadline),
        Err(FlowRefusal::Closing)
    );
    let close = ledger.close_request().unwrap();
    assert_eq!(close.destination, flow.destination());
    assert_eq!(close.flow, flow);
    assert_eq!(close.deadline, deadline);
    assert_eq!(ledger.usage(), usage);
    assert_eq!(
        ledger.confirm_closed(connection(1, 2), deadline),
        Err(FlowRefusal::WrongDestination)
    );
    drop(queued);
    assert_eq!(
        ledger.confirm_closed(flow.destination(), deadline),
        Ok(usage.total())
    );
    assert_eq!(
        ledger.confirm_closed(flow.destination(), deadline),
        Ok(Counts::ZERO)
    );
    assert_eq!(ledger.usage(), Usage::default());
    assert_eq!(ledger.tick(deadline), None);
    assert_eq!(ledger.open(&route, deadline), Err(FlowRefusal::Closed));
    assert_eq!(
        ledger.receive(sibling_flow, &route, header(&sibling, 1), deadline),
        Err(FlowRefusal::Closed)
    );
    let trace = ledger.observer();
    assert!(
        trace
            .0
            .iter()
            .any(|event| event.decision == flow::Decision::Closing(close))
    );
    assert_eq!(
        trace.0.last().unwrap().decision,
        flow::Decision::Refused(FlowRefusal::Closed)
    );
}

#[test]
fn partial_consumption_and_cleanup_retries_cannot_delay_the_oldest_retirement() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut first = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let mut second = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    let mut ledger = ledger::<2>(&fixture, first.key().sender());
    let one = open(
        &mut ledger,
        &engine,
        &first,
        Direction::ToSender,
        fixture.at,
    );
    let two = open(
        &mut ledger,
        &engine,
        &second,
        Direction::ToSender,
        fixture.at,
    );
    {
        let route = first
            .check_forward(&engine, one.permit(), fixture.at)
            .unwrap();
        let grant = ledger.grant(one, &route, counts(1), fixture.at).unwrap();
        assert_eq!(grant.credit, counts(1));
    }
    for _ in 0..2 {
        let receipt = receive(&mut ledger, &engine, &mut first, one, 1, fixture.at);
        submit(&mut ledger, &engine, &first, &receipt, fixture.at);
    }
    let receipt = receive(&mut ledger, &engine, &mut second, two, 1, fixture.at);
    submit(&mut ledger, &engine, &second, &receipt, fixture.at);
    let retire_one = cancel(&engine, &mut first, fixture.at);
    assert_eq!(ledger.retire(retire_one, fixture.at), Ok(Some(one)));
    let first_deadline = ledger.deadline().unwrap();
    let later = UnixMillis(fixture.at.0 + 1);
    let retire_two = cancel(&engine, &mut second, later);
    assert_eq!(ledger.retire(retire_two, later), Ok(Some(two)));
    let notice = ledger.discarded(one, later).unwrap().unwrap();
    let second_notice = ledger.discarded(two, later).unwrap().unwrap();
    assert_eq!(
        ledger.consume(one, one.destination(), Counts::new(1, SMALL_FRAME), later),
        Ok(Feedback::Released(Counts::new(1, SMALL_FRAME)))
    );
    assert_eq!(ledger.discarded(one, later), Ok(Some(notice)));
    assert_eq!(ledger.retire(retire_one, later), Ok(Some(one)));
    assert_eq!(ledger.deadline(), Some(first_deadline));
    assert_eq!(
        ledger.settle(one, one.destination(), notice.submitted, later),
        Ok(Feedback::Settled(Counts::new(1, SMALL_FRAME)))
    );
    assert_eq!(ledger.deadline(), Some(UnixMillis(first_deadline.0 + 1)));
    assert_eq!(ledger.tick(first_deadline), None);
    let deadline = ledger.deadline().unwrap();
    let close = ledger.tick(deadline).unwrap();
    assert_eq!(close.flow, two);
    assert_eq!(
        ledger.consume(two, two.destination(), second_notice.submitted, deadline),
        Ok(Feedback::Settled(second_notice.submitted))
    );
    assert_eq!(ledger.close_request(), Some(close));
    assert_eq!(ledger.usage().total(), Counts::ZERO);
    let mut restored = restore_ledger(&fixture, two.destination(), &ledger);
    assert_eq!(restored.close_request(), Some(close));
    assert_eq!(
        restored.confirm_closed(two.destination(), deadline),
        Ok(Counts::ZERO)
    );
    let mut closed = restore_ledger(&fixture, two.destination(), &restored);
    let replacement = join(&fixture, &mut engine, connection(4, 1), 3, deadline);
    let route = replacement
        .check_route(&engine, Direction::ToSender, deadline)
        .unwrap();
    assert_eq!(closed.open(&route, deadline), Err(FlowRefusal::Closed));
}
