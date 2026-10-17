use super::*;
use crate::{
    config::limits,
    environment::Trace,
    test_support::{Fixture, connection},
};
use windshare_relay_core::{
    flow::Counts,
    sessions::{OpaqueHeader, SessionKey},
    share::{Control, Input, Output, Responsibility, Restore, WorkResult},
};

struct Scenario {
    fixture: Fixture,
    relay: ShareRelay<Trace>,
    session: SessionKey,
    flow: FlowKey,
}

impl Scenario {
    fn new() -> Self {
        let fixture = Fixture::new();
        let (mut relay, _) = ShareRelay::restore(
            fixture.identity(),
            limits(),
            &Restore::restarted(None, Responsibility::Released),
            Trace,
            fixture.now(true),
        )
        .unwrap();
        fixture.begin(&mut relay, true);
        fixture.begin(&mut relay, false);
        let at = fixture.now(false);
        let _ = relay.handle(
            Input::Control {
                connection: connection(1),
                frame: &fixture.bytes("registerProofB64"),
            },
            at,
        );
        let mut turn = relay.handle(
            Input::Control {
                connection: connection(1),
                frame: &fixture.bytes("descriptorUploadB64"),
            },
            at,
        );
        for _ in 0..2 {
            let work = turn
                .outputs
                .into_iter()
                .find_map(|out| match out {
                    Output::Work(work) => Some(work),
                    _ => None,
                })
                .unwrap();
            turn = relay.handle(
                Input::Completed {
                    id: work.id,
                    result: WorkResult::Complete,
                },
                at,
            );
        }
        let _ = relay.handle(Input::Connected(connection(3)), at);
        let turn = relay.handle(
            Input::Control {
                connection: connection(3),
                frame: &fixture.bytes("joinB64"),
            },
            at,
        );
        let session = turn
            .outputs
            .iter()
            .find_map(|out| match out {
                Output::Send {
                    control: Control::Descriptor { session, .. },
                    ..
                } => Some(*session),
                _ => None,
            })
            .unwrap();
        let flow = turn
            .outputs
            .iter()
            .find_map(|out| match out {
                Output::Send {
                    control: Control::Grant(grant),
                    ..
                } if grant.flow.destination() == connection(3) => Some(grant.flow),
                _ => None,
            })
            .unwrap();
        Self {
            fixture,
            relay,
            session,
            flow,
        }
    }

    fn grant(&mut self, frames: u64) {
        let turn = self.relay.handle(
            Input::Grant {
                flow: self.flow,
                credit: Counts::new(frames, frames * u64::from(OpaqueHeader::MAX_ENCODED_BYTES)),
            },
            self.fixture.now(false),
        );
        assert_eq!(turn.refusal, None);
    }

    fn receive(&mut self, payload: usize) -> (ReceivedFrame, Vec<u8>) {
        let mut bytes = b"WS2O\x02\0\0\0".to_vec();
        bytes.extend_from_slice(self.session.wire_id().as_bytes());
        bytes.extend_from_slice(&u32::try_from(payload).unwrap().to_be_bytes());
        bytes.resize(20 + payload, 7);
        let turn = self.relay.handle(
            Input::Frame {
                source: connection(1),
                header: OpaqueHeader::decode(&bytes).unwrap(),
            },
            self.fixture.now(false),
        );
        assert_eq!(turn.refusal, None);
        let receipt = turn
            .outputs
            .into_iter()
            .find_map(|out| match out {
                Output::Received(receipt) => Some(receipt),
                _ => None,
            })
            .unwrap();
        (receipt, bytes)
    }

    fn queue(&mut self, outbox: &mut Outbox, payload: usize) {
        let (receipt, bytes) = self.receive(payload);
        outbox.queue(receipt, bytes).unwrap();
    }

    fn submit(&mut self, outbox: &mut Outbox) -> Option<Turn> {
        outbox.submit_next(&mut self.relay, self.fixture.now(false))
    }

    fn stop(&mut self) -> Turn {
        self.relay.handle(
            Input::Control {
                connection: connection(2),
                frame: &self.fixture.bytes("stopProofB64"),
            },
            self.fixture.now(false),
        )
    }
}

#[tokio::test]
async fn submission_is_fifo_bounded_and_old_receipts_cannot_cross_stop() {
    let mut scenario = Scenario::new();
    scenario.grant(3);
    let (mut outbox, mut writer, _) = channel(connection(3));
    for _ in 0..4 {
        scenario.queue(&mut outbox, 1);
    }
    for _ in 0..SUBMITTED_DATA_FRAMES {
        assert_eq!(scenario.submit(&mut outbox).unwrap().refusal, None);
    }
    assert!(
        scenario.submit(&mut outbox).is_none(),
        "full host mailbox must not commit"
    );
    assert_eq!(
        scenario
            .relay
            .delivery_usage(connection(3))
            .unwrap()
            .submitted
            .frames,
        2
    );
    assert_eq!(outbox.pending.len(), 2);
    // Reserved control capacity remains usable, and ordered retirement controls
    // cannot jump in front of frames that were already submitted.
    outbox.control(b"ordered-control".to_vec()).unwrap();
    let stopping = scenario.stop();
    assert!(stopping.refusal.is_none());
    writer.recv().await.unwrap();
    assert!(scenario.submit(&mut outbox).unwrap().refusal.is_some());
    assert_eq!(
        scenario
            .relay
            .delivery_usage(connection(3))
            .unwrap()
            .submitted
            .frames,
        2
    );
    let mut cleanup: VecDeque<_> = stopping.outputs.into();
    while let Some(output) = cleanup.pop_front() {
        let input = match output {
            Output::Discard(flow) => {
                outbox.discard(flow);
                Input::Discarded(flow)
            }
            Output::Work(work) => Input::Completed {
                id: work.id,
                result: WorkResult::Complete,
            },
            _ => continue,
        };
        cleanup.extend(
            scenario
                .relay
                .handle(input, scenario.fixture.now(false))
                .outputs,
        );
    }
    assert!(outbox.pending.is_empty());
    assert_eq!(outbox.pending_bytes, 0);
    assert_eq!(writer.recv().await.unwrap().len(), 21);
    assert_eq!(writer.recv().await.unwrap(), b"ordered-control");
    assert!(
        writer.try_recv().is_err(),
        "revoked callback did not hand off a frame"
    );
}

#[test]
fn byte_limit_rejects_before_growing_the_pending_queue() {
    let mut scenario = Scenario::new();
    scenario.grant(8);
    let (mut outbox, _writer, _) = channel(connection(3));
    outbox.pending_limit = Counts::new(
        DATA_QUEUE_FRAMES as u64,
        8 * u64::from(OpaqueHeader::MAX_ENCODED_BYTES),
    );
    let payload = OpaqueHeader::MAX_ENCODED_BYTES as usize - 20;
    for _ in 0..8 {
        scenario.queue(&mut outbox, payload);
    }
    let (receipt, bytes) = scenario.receive(1);
    assert!(outbox.queue(receipt, bytes).is_err());
    assert_eq!(
        outbox.pending_bytes as u64,
        outbox.pending_limit.encoded_bytes
    );
    assert!(outbox.pending.len() < DATA_QUEUE_FRAMES);
    outbox.discard(scenario.flow);
    assert!(outbox.pending.is_empty());
    assert_eq!(outbox.pending_bytes, 0);
}

#[test]
fn queue_rejects_mismatched_payload_and_destination() {
    let mut scenario = Scenario::new();
    let (mut wrong, _writer, _) = channel(connection(4));
    let (receipt, bytes) = scenario.receive(1);
    assert!(wrong.queue(receipt, bytes).is_err());
    let (mut outbox, _writer, _) = channel(connection(3));
    scenario.grant(1);
    let (receipt, _) = scenario.receive(1);
    assert!(outbox.queue(receipt, vec![1]).is_err());
}

#[tokio::test]
async fn control_capacity_includes_in_flight_and_probe_coalesces_separately() {
    let (outbox, mut writer, _) = channel(connection(3));
    for _ in 0..CONTROL_QUEUE_FRAMES {
        outbox.control(vec![1]).unwrap();
    }
    assert!(outbox.control(vec![1]).is_err());
    assert!(outbox.control(vec![0; MAX_CONTROL_BYTES + 1]).is_err());
    outbox.probe(vec![1]).unwrap();
    outbox.probe(vec![2]).unwrap();
    assert_eq!(*outbox.probe.borrow(), Some(vec![2]));
    assert_eq!(writer.recv().await.unwrap(), [1]);
    outbox.control(vec![3]).unwrap();
}

#[test]
fn frame_limit_is_independent_of_byte_capacity() {
    let mut scenario = Scenario::new();
    scenario.grant(2);
    let (mut outbox, _writer, _) = channel(connection(3));
    outbox.pending_limit.frames = 2;
    scenario.queue(&mut outbox, 1);
    scenario.queue(&mut outbox, 1);
    let (receipt, bytes) = scenario.receive(1);
    assert!(outbox.queue(receipt, bytes).is_err());
    assert_eq!(outbox.pending.len(), 2);
    assert!(outbox.pending_bytes < DATA_QUEUE_BYTES);
}

#[test]
fn byte_reservation_failure_does_not_consume_a_frame_slot_or_commit() {
    let mut scenario = Scenario::new();
    let (mut outbox, _writer, _) = channel(connection(3));
    scenario.queue(&mut outbox, 1);
    let held = Arc::clone(&outbox.data_bytes)
        .try_acquire_many_owned(u32::try_from(SUBMITTED_DATA_BYTES - 20).unwrap())
        .unwrap();
    assert!(scenario.submit(&mut outbox).is_none());
    assert_eq!(outbox.pending.len(), 1);
    assert_eq!(outbox.data_slots.available_permits(), SUBMITTED_DATA_FRAMES);
    assert_eq!(
        scenario
            .relay
            .delivery_usage(connection(3))
            .unwrap()
            .submitted,
        Counts::ZERO
    );
    drop(held);
    assert_eq!(scenario.submit(&mut outbox).unwrap().refusal, None);
}
