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
        ConnectionLedger::new(fixture.relay(), connection(1, 1), limits(), 0, ()).unwrap_err(),
        ConfigurationError::EmptyStorage
    );
}

#[test]
fn a_host_policy_alone_cannot_authorize_delivery_before_the_handshake() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let route = session
        .check_route(&engine, Direction::ToSender, fixture.at)
        .unwrap();
    let mut ledger =
        ConnectionLedger::new(fixture.relay(), session.key().sender(), limits(), 1, ()).unwrap();
    assert_eq!(
        ledger.open(&route, fixture.at),
        Err(FlowRefusal::CapacityUndeclared)
    );
    assert_eq!(ledger.usage(), Usage::default());
}

#[test]
fn unused_startup_bytes_do_not_block_admission_when_ordinary_capacity_is_free() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let mut ledger = ledger(3, &fixture, published.owner.connection());
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
fn smaller_declarations_protect_startup_capacity_through_restore() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let destination = published.owner.connection();
    let mut ledger = ConnectionLedger::new(
        fixture.relay(),
        destination,
        limits(),
        3,
        FlowTrace::default(),
    )
    .unwrap();
    ledger
        .declare_capacity(
            flow::ReceiveCapacity::new(4, 4 * MAX_FRAME).unwrap(),
            fixture.at,
        )
        .unwrap();
    let first = join(&fixture, &mut engine, connection(2, 1), 2, fixture.at);
    let route = first
        .check_route(&engine, Direction::ToSender, fixture.at)
        .unwrap();
    let flow = ledger.open(&route, fixture.at).unwrap().flow;
    let credit = ledger.window_credit(flow, counts(8)).unwrap();
    assert_eq!(credit, counts(2));
    let _ = ledger.grant(flow, &route, credit, fixture.at).unwrap();
    assert_eq!(ledger.window_credit(flow, counts(8)), Some(Counts::ZERO));
    let mut ledger = restore_ledger(&fixture, destination, &ledger);
    assert_eq!(
        ledger.grant(flow, &route, counts(1), fixture.at),
        Err(FlowRefusal::DeliveryCapacity)
    );
    let second = join(&fixture, &mut engine, connection(3, 1), 3, fixture.at);
    open(
        &mut ledger,
        &engine,
        &second,
        Direction::ToSender,
        fixture.at,
    );
    assert_eq!(ledger.usage().total(), counts(4));
}

#[test]
fn metadata_capacity_is_bounded_independently_of_available_delivery_memory() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut first = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let second = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    let mut ledger = ledger(1, &fixture, first.key().sender());
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
    let mut ledger = ConnectionLedger::new(
        fixture.relay(),
        session.key().sender(),
        settings,
        1,
        FlowTrace::default(),
    )
    .unwrap();
    ledger
        .declare_capacity(
            crate::flow::ReceiveCapacity::new(u64::MAX, u64::MAX).unwrap(),
            fixture.at,
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
    let mut ledger = ledger(1, &fixture, session.key().sender());
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

#[test]
fn shared_windows_keep_full_early_windows_and_admit_every_receiver_while_idle() {
    // Native sender ledger: 256 frames but 64 full frames of bytes.
    let limits = Limits::new(
        Counts::new(256, 64 * MAX_FRAME),
        counts(2),
        counts(1),
        SETTLEMENT_TIMEOUT,
    )
    .unwrap();
    let share = flow::WindowShare {
        floor: Counts::new(2, MAX_FRAME),
        flows: 31,
    };
    let target = counts(16);
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let mut ledger = ConnectionLedger::new(
        fixture.relay(),
        published.owner.connection(),
        limits,
        64,
        FlowTrace::default(),
    )
    .unwrap();
    ledger
        .declare_capacity(
            flow::ReceiveCapacity::new(256, 16 << 20).unwrap(),
            fixture.at,
        )
        .unwrap();
    for id in 2..2 + share.flows {
        let mut session = join(&fixture, &mut engine, connection(id, 1), id, fixture.at);
        let flow = open(
            &mut ledger,
            &engine,
            &session,
            Direction::ToSender,
            fixture.at,
        );
        let credit = ledger.shared_window_credit(flow, target, share).unwrap();
        if id < 4 {
            // A lone receiver, and the next, keep today's full window.
            assert_eq!(credit, counts(15), "receiver {id}");
        }
        assert!(
            credit.frames >= 1,
            "receiver {id} cannot refill to its floor"
        );
        grant(&mut ledger, &engine, &session, flow, credit, fixture.at);
        // One small envelope is consumed, then the receiver idles on its credit.
        let receipt = receive(&mut ledger, &engine, &mut session, flow, 1, fixture.at);
        submit(&mut ledger, &engine, &session, &receipt, fixture.at);
        let _ = ledger
            .consume(
                flow,
                flow.destination(),
                Counts::new(1, SMALL_FRAME),
                fixture.at,
            )
            .unwrap();
        let credit = ledger.shared_window_credit(flow, target, share).unwrap();
        grant(&mut ledger, &engine, &session, flow, credit, fixture.at);
        let occupied = ledger.snapshot(flow).unwrap().usage.total();
        assert!(occupied.frames >= share.floor.frames, "receiver {id}");
    }
    // The plain window would have exhausted ordinary bytes after four receivers.
    assert!(ledger.usage().total().encoded_bytes <= 64 * MAX_FRAME);
}

#[test]
fn shared_windows_hold_every_joined_floor_whatever_the_refill_rotation() {
    // Like the native host, offer credit to every open flow after each event,
    // oldest first, so earlier siblings are always asked before the newcomer.
    let limits = Limits::new(
        Counts::new(256, 64 * MAX_FRAME),
        counts(2),
        counts(1),
        SETTLEMENT_TIMEOUT,
    )
    .unwrap();
    let share = flow::WindowShare {
        floor: Counts::new(2, MAX_FRAME),
        flows: 31,
    };
    let target = counts(16);
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let mut ledger = ConnectionLedger::new(
        fixture.relay(),
        published.owner.connection(),
        limits,
        64,
        FlowTrace::default(),
    )
    .unwrap();
    ledger
        .declare_capacity(
            flow::ReceiveCapacity::new(256, 16 << 20).unwrap(),
            fixture.at,
        )
        .unwrap();
    let mut flows = Vec::new();
    let mut sessions = Vec::new();
    let refill = |ledger: &mut ConnectionLedger<FlowTrace>,
                  engine: &ShareLifecycle<Recorder>,
                  sessions: &[Session],
                  flows: &[FlowKey]| {
        for (session, flow) in sessions.iter().zip(flows) {
            let credit = ledger.shared_window_credit(*flow, target, share).unwrap();
            grant(ledger, engine, session, *flow, credit, fixture.at);
        }
    };
    for id in 2..2 + share.flows {
        sessions.push(join(
            &fixture,
            &mut engine,
            connection(id, 1),
            id,
            fixture.at,
        ));
        let session = sessions.last().unwrap();
        flows.push(open(
            &mut ledger,
            &engine,
            session,
            Direction::ToSender,
            fixture.at,
        ));
        let flow = *flows.last().unwrap();
        refill(&mut ledger, &engine, &sessions, &flows);
        let session = sessions.last_mut().unwrap();
        let receipt = receive(&mut ledger, &engine, session, flow, 1, fixture.at);
        submit(&mut ledger, &engine, session, &receipt, fixture.at);
        let _ = ledger
            .consume(
                flow,
                flow.destination(),
                Counts::new(1, SMALL_FRAME),
                fixture.at,
            )
            .unwrap();
        refill(&mut ledger, &engine, &sessions, &flows);
    }
    for (index, flow) in flows.iter().enumerate() {
        let occupied = ledger.snapshot(*flow).unwrap().usage.total();
        assert!(
            occupied.frames >= share.floor.frames
                && occupied.encoded_bytes >= share.floor.encoded_bytes,
            "receiver {index} below its floor: {occupied:?}, ledger {:?}",
            ledger.usage().total()
        );
    }
}

fn grant(
    ledger: &mut ConnectionLedger<FlowTrace>,
    engine: &ShareLifecycle<Recorder>,
    session: &Session,
    flow: FlowKey,
    credit: Counts,
    at: UnixMillis,
) {
    if credit != Counts::ZERO {
        let route = session
            .check_route(engine, Direction::ToSender, at)
            .unwrap();
        let _ = ledger.grant(flow, &route, credit, at).unwrap();
    }
}
