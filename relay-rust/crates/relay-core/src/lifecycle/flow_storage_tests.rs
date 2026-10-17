use super::*;

const FANOUT: usize = 128;

#[test]
fn metadata_allocation_tracks_fanout_instead_of_repeating_it_per_connection() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let budget = counts(FANOUT as u64);
    let settings = Limits::new(budget, budget, counts(1), SETTLEMENT_TIMEOUT).unwrap();
    let new_ledger = |destination| {
        let mut ledger =
            ConnectionLedger::new(fixture.relay(), destination, settings, FANOUT, ()).unwrap();
        assert_eq!(ledger.metadata_usage().allocated_slots, 0);
        ledger
            .declare_capacity(
                flow::ReceiveCapacity::new(budget.frames, budget.encoded_bytes).unwrap(),
                fixture.at,
            )
            .unwrap();
        ledger
    };
    let mut sender = new_ledger(published.owner.connection());
    let mut receivers = Vec::new();
    for id in 1..=FANOUT as u64 {
        let session = join(&fixture, &mut engine, connection(id + 1, 1), id, fixture.at);
        let mut receiver = new_ledger(session.key().receiver());
        for (ledger, direction) in [
            (&mut sender, Direction::ToSender),
            (&mut receiver, Direction::ToReceiver),
        ] {
            let route = session.check_route(&engine, direction, fixture.at).unwrap();
            let _ = ledger.open(&route, fixture.at).unwrap();
        }
        assert_eq!(receiver.metadata_usage().retained_flows, 1);
        assert_eq!(receiver.metadata_usage().allocated_slots, 1);
        receivers.push(receiver);
    }
    let allocated = sender.metadata_usage().allocated_slots
        + receivers
            .iter()
            .map(|ledger| ledger.metadata_usage().allocated_slots)
            .sum::<usize>();
    assert_eq!(allocated, FANOUT * 2);
    assert_eq!(sender.metadata_usage().retained_flows, FANOUT);
    sender
        .confirm_closed(published.owner.connection(), fixture.at)
        .unwrap();
    assert_eq!(sender.metadata_usage().allocated_slots, 0);
}

#[test]
fn growth_and_slot_reuse_preserve_live_handles_and_report_metadata_pressure() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut sessions: Vec<_> = (1..=4)
        .map(|id| join(&fixture, &mut engine, connection(id + 1, 1), id, fixture.at))
        .collect();
    let mut ledger = ledger(3, &fixture, sessions[0].key().sender());
    let mut keys = Vec::new();
    for session in &sessions[..3] {
        keys.push(open(
            &mut ledger,
            &engine,
            session,
            Direction::ToSender,
            fixture.at,
        ));
        for key in &keys {
            assert!(ledger.snapshot(*key).is_some());
        }
        assert!(ledger.metadata_usage().allocated_slots <= ledger.metadata_usage().limit);
    }
    let route = sessions[3]
        .check_route(&engine, Direction::ToSender, fixture.at)
        .unwrap();
    assert_eq!(
        ledger.open(&route, fixture.at),
        Err(FlowRefusal::FlowCapacity)
    );
    let refusal = ledger.observer().0.last().unwrap();
    assert_eq!(refusal.metadata.retained_flows, 3);
    assert_eq!(refusal.metadata.limit, 3);
    let retired = cancel(&engine, &mut sessions[1], fixture.at);
    ledger.retire(retired, fixture.at).unwrap();
    ledger.discarded(keys[1], fixture.at).unwrap();
    let route = sessions[3]
        .check_route(&engine, Direction::ToSender, fixture.at)
        .unwrap();
    let replacement = ledger.open(&route, fixture.at).unwrap().flow;
    assert_eq!(ledger.metadata_usage().allocated_slots, 3);
    assert_eq!(
        ledger.consume(keys[1], keys[1].destination(), Counts::ZERO, fixture.at),
        Ok(Feedback::Stale)
    );
    for key in [keys[0], keys[2], replacement] {
        assert!(ledger.snapshot(key).is_some());
    }
    for index in [0, 2, 3] {
        let retirement = cancel(&engine, &mut sessions[index], fixture.at);
        let key = ledger.retire(retirement, fixture.at).unwrap().unwrap();
        ledger.discarded(key, fixture.at).unwrap();
    }
    assert_eq!(ledger.metadata_usage().retained_flows, 0);
    assert_eq!(ledger.metadata_usage().allocated_slots, 0);
}

#[test]
fn sparse_checkpoint_preserves_indices_retirement_and_late_feedback() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut first = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let mut second = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    let third = join(&fixture, &mut engine, connection(4, 1), 3, fixture.at);
    let mut ledger = ledger(FANOUT, &fixture, first.key().sender());
    let first_key = open(
        &mut ledger,
        &engine,
        &first,
        Direction::ToSender,
        fixture.at,
    );
    let single_record_bytes = ledger.checkpoint().unwrap().encode().len();
    let second_key = open(
        &mut ledger,
        &engine,
        &second,
        Direction::ToSender,
        fixture.at,
    );
    let receipt = receive(&mut ledger, &engine, &mut second, second_key, 1, fixture.at);
    submit(&mut ledger, &engine, &second, &receipt, fixture.at);
    for (session, key) in [(&mut first, first_key), (&mut second, second_key)] {
        ledger
            .retire(cancel(&engine, session, fixture.at), fixture.at)
            .unwrap();
        ledger.discarded(key, fixture.at).unwrap();
    }
    assert!(ledger.snapshot(first_key).is_none());
    assert_eq!(ledger.metadata_usage().retained_flows, 1);
    let checkpoint = ledger.checkpoint().unwrap();
    let bytes = checkpoint.encode();
    assert_eq!(bytes.len(), single_record_bytes);
    let checkpoint = flow::LedgerCheckpoint::decode(&bytes, FANOUT).unwrap();
    // The old compile-time capacity is not part of the snapshot. Only the
    // retained index and actual responsibility constrain a new runtime bound.
    let mut restored = ConnectionLedger::restore(
        fixture.relay(),
        second_key.destination(),
        limits(),
        2,
        &checkpoint,
        FlowTrace::default(),
    )
    .unwrap();
    assert_eq!(restored.snapshot(second_key), ledger.snapshot(second_key));
    assert_eq!(restored.deadline(), ledger.deadline());
    assert_eq!(restored.usage(), ledger.usage());
    assert_eq!(restored.metadata_usage().allocated_slots, 2);
    assert_eq!(
        flow::LedgerCheckpoint::decode(&bytes, 1),
        Err(flow::CheckpointError::Capacity)
    );
    assert!(matches!(
        ConnectionLedger::restore(
            fixture.relay(),
            second_key.destination(),
            limits(),
            1,
            &checkpoint,
            ()
        ),
        Err(flow::CheckpointError::Capacity)
    ));
    let third_key = open(
        &mut restored,
        &engine,
        &third,
        Direction::ToSender,
        fixture.at,
    );
    assert_eq!(restored.discarded(first_key, fixture.at), Ok(None));
    assert_eq!(
        restored.consume(first_key, first_key.destination(), Counts::ZERO, fixture.at),
        Ok(Feedback::Stale)
    );
    assert_eq!(
        restored.settle(
            second_key,
            second_key.destination(),
            Counts::new(1, SMALL_FRAME),
            fixture.at
        ),
        Ok(Feedback::Settled(Counts::new(1, SMALL_FRAME)))
    );
    assert!(restored.snapshot(third_key).is_some());
    assert_eq!(restored.deadline(), None);
}

#[test]
fn sparse_checkpoint_rejects_duplicate_unordered_out_of_bound_and_truncated_records() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let first = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let second = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    let mut ledger = ledger(FANOUT, &fixture, first.key().sender());
    let empty = ledger.checkpoint().unwrap().encode();
    open(
        &mut ledger,
        &engine,
        &first,
        Direction::ToSender,
        fixture.at,
    );
    let entry_bytes = ledger.checkpoint().unwrap().encode().len() - empty.len();
    open(
        &mut ledger,
        &engine,
        &second,
        Direction::ToSender,
        fixture.at,
    );
    let encoded = ledger.checkpoint().unwrap().encode();
    let first_slot = empty.len();
    let second_slot = first_slot + entry_bytes;
    for (slot, value) in [
        (second_slot, 0_u64),
        (first_slot, 2),
        (second_slot, FANOUT as u64),
        (first_slot, u64::MAX),
    ] {
        let mut invalid = encoded.clone();
        invalid[slot..slot + size_of::<u64>()].copy_from_slice(&value.to_be_bytes());
        assert!(flow::LedgerCheckpoint::decode(&invalid, FANOUT).is_err());
    }
    let mut oversized = empty;
    let count_offset = oversized.len() - size_of::<u64>();
    oversized[count_offset..].copy_from_slice(&u64::MAX.to_be_bytes());
    assert_eq!(
        flow::LedgerCheckpoint::decode(&oversized, FANOUT),
        Err(flow::CheckpointError::Capacity)
    );
    assert!(flow::LedgerCheckpoint::decode(&encoded[..encoded.len() - 1], FANOUT).is_err());
    let mut trailing = encoded.clone();
    trailing.push(0);
    assert!(flow::LedgerCheckpoint::decode(&trailing, FANOUT).is_err());
    assert!(encoded.len() <= flow::LedgerCheckpoint::maximum_bytes(2).unwrap());
    assert_eq!(flow::LedgerCheckpoint::maximum_bytes(usize::MAX), None);
}
