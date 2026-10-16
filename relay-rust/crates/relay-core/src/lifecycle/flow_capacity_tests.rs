use super::*;

#[test]
fn capacity_configuration_requires_room_for_startup_and_bounded_cleanup() {
    assert_eq!(
        Limits::new(
            counts(4),
            counts(1),
            Counts::new(0, MAX_FRAME),
            SETTLEMENT_TIMEOUT
        ),
        Err(ConfigurationError::StartupTooSmall)
    );
    assert_eq!(
        Limits::new(
            counts(4),
            counts(1),
            Counts::new(1, MAX_FRAME - 1),
            SETTLEMENT_TIMEOUT
        ),
        Err(ConfigurationError::StartupTooSmall)
    );
    assert_eq!(
        Limits::new(counts(1), counts(2), counts(1), SETTLEMENT_TIMEOUT),
        Err(ConfigurationError::CapacityOrder)
    );
    assert_eq!(
        Limits::new(counts(4), counts(1), counts(2), SETTLEMENT_TIMEOUT),
        Err(ConfigurationError::CapacityOrder)
    );
    assert_eq!(
        Limits::new(counts(4), counts(2), counts(1), Duration::ZERO),
        Err(ConfigurationError::EmptyTimeout)
    );
    assert_eq!(limits().startup_reserve(), counts(2));
    assert_eq!(limits().startup_grant(), counts(1));
    let fixture = Fixture::new();
    assert_eq!(
        ConnectionLedger::<0, ()>::new(fixture.relay(), connection(1, 1), limits(), ())
            .unwrap_err(),
        ConfigurationError::EmptyStorage
    );
}

#[test]
fn unused_startup_bytes_do_not_block_admission_when_ordinary_capacity_is_free() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let owner = fixture.register(&mut engine);
    let mut ledger = ledger::<3>(&fixture, owner.id.connection());
    for id in 2..4 {
        let mut session = join(&fixture, &mut engine, connection(id, 1), id, fixture.at);
        let flow = open(
            &mut ledger,
            &engine,
            &session,
            Direction::ToSender,
            fixture.at,
        );
        let receipt = receive(&mut ledger, &engine, &mut session, flow, 1, fixture.at);
        submit(&mut ledger, &engine, &session, &receipt, fixture.at);
        assert_eq!(
            ledger.consume(
                flow,
                flow.destination(),
                Counts::new(1, SMALL_FRAME),
                fixture.at
            ),
            Ok(Feedback::Released(Counts::new(1, SMALL_FRAME)))
        );
    }
    let retained_bytes = MAX_FRAME * 2 - SMALL_FRAME * 2;
    assert_eq!(ledger.startup_used(), Counts::new(0, retained_bytes));
    assert_eq!(ledger.regular_used(), Counts::ZERO);
    let mut newcomer = join(&fixture, &mut engine, connection(4, 1), 4, fixture.at);
    let flow = open(
        &mut ledger,
        &engine,
        &newcomer,
        Direction::ToSender,
        fixture.at,
    );
    assert_eq!(ledger.startup_used(), Counts::new(1, MAX_FRAME * 2));
    assert_eq!(
        ledger.regular_used(),
        Counts::new(0, MAX_FRAME - SMALL_FRAME * 2)
    );
    let receipt = receive(
        &mut ledger,
        &engine,
        &mut newcomer,
        flow,
        sessions::MAX_OPAQUE_CIPHERTEXT_BYTES,
        fixture.at,
    );
    submit(&mut ledger, &engine, &newcomer, &receipt, fixture.at);
    assert_eq!(
        ledger.consume(flow, flow.destination(), counts(1), fixture.at),
        Ok(Feedback::Released(counts(1)))
    );
    assert_eq!(ledger.startup_used(), Counts::new(0, retained_bytes));
    assert_eq!(ledger.regular_used(), Counts::ZERO);
}

#[test]
fn metadata_capacity_is_bounded_independently_of_available_delivery_memory() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut first = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let second = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    let mut ledger = ledger::<1>(&fixture, first.key().sender());
    let flow = open(
        &mut ledger,
        &engine,
        &first,
        Direction::ToSender,
        fixture.at,
    );
    let other = second
        .check_route(&engine, Direction::ToSender, fixture.at)
        .unwrap();
    assert_eq!(
        ledger.open(&other, fixture.at),
        Err(FlowRefusal::FlowCapacity)
    );
    let retirement = cancel(&engine, &mut first, fixture.at);
    assert_eq!(ledger.retire(retirement, fixture.at), Ok(Some(flow)));
    assert_eq!(
        ledger.open(&other, fixture.at),
        Err(FlowRefusal::FlowCapacity)
    );
    let notice = ledger.discarded(flow, fixture.at).unwrap().unwrap();
    assert_eq!(notice.submitted, Counts::ZERO);
    assert_eq!(ledger.open(&other, fixture.at).unwrap().credit, counts(1));
}

#[test]
fn cumulative_counters_cannot_wrap_after_capacity_is_reclaimed() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let total = Counts::new(u64::MAX, u64::MAX);
    let settings = Limits::new(total, counts(1), counts(1), SETTLEMENT_TIMEOUT).unwrap();
    let mut ledger = ConnectionLedger::<1, _>::new(
        fixture.relay(),
        session.key().sender(),
        settings,
        FlowTrace::default(),
    )
    .unwrap();
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
        assert_eq!(
            ledger.grant(flow, &route, Counts::ZERO, fixture.at),
            Err(FlowRefusal::EmptyGrant)
        );
        let grant = ledger
            .grant(
                flow,
                &route,
                Counts::new(u64::MAX - 1, u64::MAX - MAX_FRAME),
                fixture.at,
            )
            .unwrap();
        assert_eq!(grant.credit.frames, u64::MAX - 1);
    }
    let receipt = receive(&mut ledger, &engine, &mut session, flow, 1, fixture.at);
    submit(&mut ledger, &engine, &session, &receipt, fixture.at);
    assert_eq!(
        ledger.consume(
            flow,
            flow.destination(),
            Counts::new(1, SMALL_FRAME),
            fixture.at
        ),
        Ok(Feedback::Released(Counts::new(1, SMALL_FRAME)))
    );
    let route = session
        .check_forward(&engine, flow.permit(), fixture.at)
        .unwrap();
    let before = ledger.usage();
    for credit in [Counts::new(1, 0), Counts::new(0, 1)] {
        assert_eq!(
            ledger.grant(flow, &route, credit, fixture.at),
            Err(FlowRefusal::CounterExhausted)
        );
        assert_eq!(ledger.usage(), before);
    }
}

#[test]
fn startup_and_regular_charges_follow_fifo_prefixes_across_varying_frame_sizes() {
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
    let payloads = [1, 100, sessions::MAX_OPAQUE_CIPHERTEXT_BYTES, 1_000, 2, 500];
    {
        let route = session
            .check_forward(&engine, flow.permit(), fixture.at)
            .unwrap();
        let grant = ledger.grant(flow, &route, counts(5), fixture.at).unwrap();
        assert_eq!(grant.credit, counts(5));
    }
    let mut submitted = Counts::ZERO;
    let mut cumulative = Vec::new();
    for payload in payloads {
        let receipt = receive(
            &mut ledger,
            &engine,
            &mut session,
            flow,
            payload,
            fixture.at,
        );
        submitted.frames += 1;
        submitted.encoded_bytes += receipt.encoded_bytes();
        submit(&mut ledger, &engine, &session, &receipt, fixture.at);
        cumulative.push(submitted);
        assert_eq!(ledger.usage().total(), counts(6));
    }
    for consumed in cumulative {
        let result = ledger
            .consume(flow, flow.destination(), consumed, fixture.at)
            .unwrap();
        assert!(matches!(result, Feedback::Released(_)));
        assert_eq!(
            ledger.usage().total(),
            Counts::new(6 - consumed.frames, MAX_FRAME * 6 - consumed.encoded_bytes)
        );
        assert_eq!(
            ledger.startup_used(),
            Counts::new(
                1_u64.saturating_sub(consumed.frames),
                MAX_FRAME.saturating_sub(consumed.encoded_bytes)
            )
        );
        assert_eq!(
            ledger.consume(flow, flow.destination(), consumed, fixture.at),
            Ok(Feedback::Unchanged)
        );
    }
    let retirement = cancel(&engine, &mut session, fixture.at);
    assert_eq!(ledger.retire(retirement, fixture.at), Ok(Some(flow)));
    assert_eq!(ledger.usage().total(), Counts::ZERO);
    assert_eq!(
        ledger
            .discarded(flow, fixture.at)
            .unwrap()
            .unwrap()
            .submitted,
        submitted
    );
    assert_eq!(ledger.startup_used(), Counts::ZERO);
    assert_eq!(ledger.regular_used(), Counts::ZERO);
}
