use super::*;
use crate::flow::{DeliveryConsumed, Refusal, SettlementAck, SettlementNotice};

const ONE_FRAME: Counts = Counts::new(1, 21);
const TWO_FRAMES: Counts = Counts::new(2, 42);

fn wire(relay: &mut Relay, connection: ConnectionRef, frame: &[u8], at: UnixMillis) -> Turn {
    relay.handle(Input::Control { connection, frame }, at)
}

fn submit_one(relay: &mut Relay, key: SessionKey, at: UnixMillis) -> FlowKey {
    let receipt = receive(relay, key, at);
    assert_eq!(
        relay.submit(&receipt, at, |_| Submission::Complete).refusal,
        None
    );
    receipt.flow()
}

fn cancel(relay: &mut Relay, key: SessionKey, at: UnixMillis) {
    assert_eq!(
        relay
            .handle(
                Input::Cancel {
                    participant: key.receiver(),
                    session: key.wire_id()
                },
                at
            )
            .refusal,
        None
    );
}

fn discard(relay: &mut Relay, flows: &[FlowKey; 2], at: UnixMillis) -> Vec<SettlementNotice> {
    let mut notices = Vec::new();
    for flow in flows {
        let turn = relay.handle(Input::Discarded(*flow), at);
        assert_eq!(turn.refusal, None);
        for output in turn.outputs {
            if let Output::Send {
                connection,
                control: Control::Settlement(notice),
            } = output
            {
                assert_eq!(connection, flow.destination());
                notices.push(SettlementNotice::decode(&notice.encode()).unwrap());
            }
        }
    }
    notices
}

#[test]
fn wire_ack_requires_retirement_discard_and_exact_frozen_totals() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    let flow = submit_one(&mut relay, key, fixture.at);
    assert_eq!(
        relay
            .handle(
                Input::Grant {
                    flow,
                    credit: ONE_FRAME
                },
                fixture.at
            )
            .refusal,
        None
    );
    submit_one(&mut relay, key, fixture.at);
    let ack = SettlementAck::new(key.wire_id(), TWO_FRAMES)
        .unwrap()
        .encode();
    assert_eq!(
        wire(&mut relay, key.sender(), &ack, fixture.at).refusal,
        Some(Error::Flow(Refusal::NotRetired))
    );
    let consumed = DeliveryConsumed::new(key.wire_id(), ONE_FRAME)
        .unwrap()
        .encode();
    assert_eq!(
        wire(&mut relay, key.sender(), &consumed, fixture.at).refusal,
        None
    );
    cancel(&mut relay, key, fixture.at);
    assert_eq!(
        wire(&mut relay, key.sender(), &ack, fixture.at).refusal,
        Some(Error::Flow(Refusal::CleanupPending))
    );
    let notices = discard(&mut relay, &flows, fixture.at);
    assert_eq!(notices.len(), 2);
    assert!(
        notices
            .iter()
            .all(|notice| notice.session() == key.wire_id())
    );
    assert!(notices.iter().any(|notice| notice.total() == TWO_FRAMES));
    assert!(notices.iter().any(|notice| notice.total() == Counts::ZERO));
    let before = relay.connections[&key.sender()]
        .ledger
        .snapshot(flow)
        .unwrap();
    assert_eq!(before.usage.submitted, ONE_FRAME);
    for total in [
        Counts::ZERO,
        ONE_FRAME,
        Counts::new(2, 43),
        Counts::new(3, 63),
    ] {
        let wrong = SettlementAck::new(key.wire_id(), total).unwrap().encode();
        assert_eq!(
            wire(&mut relay, key.sender(), &wrong, fixture.at).refusal,
            Some(Error::Flow(Refusal::InvalidSettlement))
        );
        assert_eq!(
            relay.connections[&key.sender()].ledger.snapshot(flow),
            Some(before)
        );
    }
    assert!(matches!(
        wire(&mut relay, key.sender(), &ack[..ack.len() - 1], fixture.at).refusal,
        Some(Error::Frame(_))
    ));
    let wrong_generation = ConnectionRef {
        generation: NonZeroU64::new(2).unwrap(),
        ..key.sender()
    };
    assert_eq!(
        wire(&mut relay, wrong_generation, &ack, fixture.at).refusal,
        Some(Error::UnknownConnection)
    );
    assert_eq!(
        relay.connections[&key.sender()].ledger.snapshot(flow),
        Some(before)
    );
    let settled = wire(&mut relay, key.sender(), &ack, fixture.at);
    assert_eq!(settled.refusal, None);
    assert!(
        !settled
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(target) if *target == key.sender()))
    );
    assert!(
        relay.connections[&key.sender()]
            .ledger
            .snapshot(flow)
            .is_none()
    );
}

#[test]
fn consumed_cannot_prove_queue_discard_and_replayed_ack_cannot_touch_reused_slots() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    let flow = submit_one(&mut relay, key, fixture.at);
    assert_eq!(
        relay
            .handle(
                Input::Grant {
                    flow,
                    credit: TWO_FRAMES
                },
                fixture.at
            )
            .refusal,
        None
    );
    submit_one(&mut relay, key, fixture.at);
    let queued = receive(&mut relay, key, fixture.at);
    cancel(&mut relay, key, fixture.at);
    let consumed = DeliveryConsumed::new(key.wire_id(), ONE_FRAME)
        .unwrap()
        .encode();
    assert_eq!(
        wire(&mut relay, key.sender(), &consumed, fixture.at).refusal,
        None
    );
    let retained = relay.connections[&key.sender()]
        .ledger
        .snapshot(flow)
        .unwrap();
    assert_eq!(retained.usage.received, ONE_FRAME);
    assert_eq!(retained.usage.submitted, ONE_FRAME);
    drop(queued);
    let notices = discard(&mut relay, &flows, fixture.at);
    assert!(notices.iter().any(|notice| notice.total() == TWO_FRAMES));
    let ack = SettlementAck::new(key.wire_id(), TWO_FRAMES)
        .unwrap()
        .encode();
    assert_eq!(
        wire(&mut relay, key.sender(), &ack, fixture.at).refusal,
        None
    );
    assert!(
        relay.connections[&key.sender()]
            .ledger
            .snapshot(flow)
            .is_none()
    );
    for _ in 0..2 {
        let duplicate = wire(&mut relay, key.sender(), &ack, fixture.at);
        assert_eq!(duplicate.refusal, None);
        assert!(duplicate.outputs.is_empty());
    }
    let (next, _) = fixture.join(&mut relay, 3);
    let next_flow = submit_one(&mut relay, next, fixture.at);
    let before = relay.connections[&next.sender()].ledger.snapshot(next_flow);
    let stale = wire(&mut relay, key.sender(), &ack, fixture.at);
    assert_eq!(stale.refusal, None);
    assert!(stale.outputs.is_empty());
    assert_eq!(
        relay.connections[&next.sender()].ledger.snapshot(next_flow),
        before
    );
    assert_ne!(next.wire_id(), key.wire_id());
}

#[test]
fn hibernated_wire_ack_after_deadline_cannot_reopen_a_closing_connection() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    submit_one(&mut relay, key, fixture.at);
    cancel(&mut relay, key, fixture.at);
    discard(&mut relay, &flows, fixture.at);
    let deadline = relay.deadline().unwrap();
    let checkpoint =
        Checkpoint::decode(&relay.checkpoint().unwrap().encode(), Fixture::limits()).unwrap();
    let (mut restored, _) = Relay::restore(
        fixture.identity(2),
        Fixture::limits(),
        &Restore {
            record: Some(stored),
            surviving: &[key.sender(), key.receiver()],
            checkpoint: Some(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert_eq!(restored.deadline(), Some(deadline));
    let ack = SettlementAck::new(key.wire_id(), ONE_FRAME)
        .unwrap()
        .encode();
    let late = wire(&mut restored, key.sender(), &ack, deadline);
    assert_eq!(late.refusal, None);
    assert!(
        late.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(target) if *target == key.sender()))
    );
    assert!(
        restored.connections[&key.sender()]
            .ledger
            .close_request()
            .is_some()
    );
    assert!(matches!(
        restored.connections[&key.sender()].role,
        Role::Closing
    ));
    let _ = restored.handle(Input::Closed(key.sender()), deadline);
    assert_eq!(
        wire(&mut restored, key.sender(), &ack, deadline).refusal,
        Some(Error::UnknownConnection)
    );
}
