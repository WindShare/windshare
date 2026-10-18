use super::*;
use crate::flow::{ReceiveCapacity, Refusal, Usage};

const MAX_FRAME: u64 = OpaqueHeader::MAX_ENCODED_BYTES as u64;

fn join_capacity(fixture: &Fixture, relay: &mut Relay, receiver: u64, capacity: Counts) -> Turn {
    let mut frame = fixture.frame("joinB64");
    let offset = frame.len() - ReceiveCapacity::ENCODED_BYTES;
    frame[offset..offset + 8].copy_from_slice(&capacity.frames.to_be_bytes());
    frame[offset + 8..].copy_from_slice(&capacity.encoded_bytes.to_be_bytes());
    assert_eq!(
        relay
            .handle(Input::Connected(connection(receiver)), fixture.at)
            .refusal,
        None
    );
    relay.handle(
        Input::Control {
            connection: connection(receiver),
            frame: &frame,
        },
        fixture.at,
    )
}

fn grants(turn: &Turn) -> Vec<crate::flow::Grant> {
    turn.outputs
        .iter()
        .filter_map(|output| match output {
            Output::Send {
                control: Control::Grant(grant),
                ..
            } => Some(*grant),
            _ => None,
        })
        .collect()
}

#[test]
fn insufficient_receiver_capacity_rolls_back_both_startup_directions() {
    let fixture = Fixture::new();
    let mut limits = Fixture::limits();
    let startup = Counts::new(2, 2 * MAX_FRAME);
    limits.delivery = crate::flow::Limits::new(
        limits.delivery.total(),
        limits.delivery.startup_reserve(),
        startup,
        Duration::from_millis(SETTLEMENT_BUDGET),
    )
    .unwrap();
    let (mut relay, _) = Relay::restore(
        fixture.identity(1),
        limits,
        &Restore::restarted(None, Responsibility::Released { previous: None }),
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    fixture.register(&mut relay);
    for (receiver, capacity) in [
        (2, Counts::new(1, 2 * MAX_FRAME)),
        (3, Counts::new(2, 2 * MAX_FRAME - 1)),
    ] {
        let denied = join_capacity(&fixture, &mut relay, receiver, capacity);
        assert_eq!(grants(&denied), []);
        assert!(denied.outputs.iter().any(
            |output| matches!(output, Output::Close(value) if *value == connection(receiver))
        ));
        for destination in [connection(1), connection(receiver)] {
            assert_eq!(ledger_of(&relay, destination).usage(), Usage::default());
            assert_eq!(ledger_of(&relay, destination).checkpoint_flows().count(), 0);
        }
    }
    assert_eq!(
        grants(&join_capacity(&fixture, &mut relay, 4, startup)).len(),
        2
    );
}

#[test]
fn asymmetric_connection_capacity_and_host_policy_both_bound_grants() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let turn = join_capacity(&fixture, &mut relay, 2, Counts::new(2, 2 * MAX_FRAME));
    let initial = grants(&turn);
    assert_eq!(initial.len(), 2);
    let [up, down] = [initial[0].flow, initial[1].flow];
    for (flow, credit) in [
        (down, Counts::new(1, MAX_FRAME)),
        (up, Counts::new(8, 8 * MAX_FRAME)),
    ] {
        assert_eq!(
            relay
                .handle(Input::Grant { flow, credit }, fixture.at)
                .refusal,
            None
        );
    }
    for flow in [up, down] {
        for credit in [Counts::new(1, 0), Counts::new(0, 1)] {
            let before = ledger_of(&relay, flow.destination()).usage();
            assert_eq!(
                relay
                    .handle(Input::Grant { flow, credit }, fixture.at)
                    .refusal,
                Some(Error::Flow(Refusal::DeliveryCapacity))
            );
            assert_eq!(ledger_of(&relay, flow.destination()).usage(), before);
        }
    }
    // Two one-frame receivers can share a sender; declarations do not resize the share.
    let another = join_capacity(&fixture, &mut relay, 3, Counts::new(1, MAX_FRAME));
    assert_eq!(grants(&another).len(), 2);
}

#[test]
fn hibernation_retains_each_capacity_and_all_outstanding_budget() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let turn = join_capacity(&fixture, &mut relay, 2, Counts::new(1, MAX_FRAME));
    let grants = grants(&turn);
    let [up, down] = [grants[0].flow, grants[1].flow];
    let receipt = receive(&mut relay, up.session(), fixture.at);
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    activate(&mut relay, up.session(), fixture.at);
    let before = ledger_of(&relay, up.destination()).usage();
    let checkpoint =
        Checkpoint::decode(&relay.checkpoint().unwrap().encode(), Fixture::limits()).unwrap();
    let (mut restored, turn) = Relay::restore(
        fixture.identity(2),
        Fixture::limits(),
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[connection(1), connection(2)],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert!(turn.outputs.is_empty());
    assert_eq!(ledger_of(&restored, up.destination()).usage(), before);
    assert_eq!(before.submitted, Counts::new(1, 21));
    assert_eq!(
        ledger_of(&restored, down.destination())
            .receive_capacity()
            .unwrap()
            .counts(),
        Counts::new(1, MAX_FRAME)
    );
    assert_eq!(
        restored
            .handle(
                Input::Grant {
                    flow: down,
                    credit: Counts::new(1, 0)
                },
                fixture.at
            )
            .refusal,
        Some(Error::Flow(Refusal::DeliveryCapacity))
    );
    restored
        .generations
        .update_ledger(
            down.destination(),
            |ledger| {
                let original = ledger.receive_capacity().unwrap();
                let usage = ledger.usage();
                assert_eq!(ledger.declare_capacity(original, fixture.at), Ok(()));
                assert_eq!(
                    ledger.declare_capacity(
                        ReceiveCapacity::new(2, 2 * MAX_FRAME).unwrap(),
                        fixture.at
                    ),
                    Err(Refusal::CapacityChanged)
                );
                assert_eq!(ledger.usage(), usage);
            },
            &mut restored.checkpointing,
            &mut restored.deadlines,
            &mut restored.observer,
        )
        .unwrap();
}

#[test]
fn stop_and_unclassified_connections_never_acquire_delivery_capacity() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let _ = relay.handle(Input::Connected(connection(1)), fixture.at);
    assert!(relay.generations.ledger(&connection(1)).is_none());
    fixture.prepare_stop(&mut relay);
    assert!(relay.generations.ledger(&connection(99)).is_none());
}
