use super::*;
use crate::{
    Database,
    config::{CONNECTION_DATA_BYTES, CONNECTION_DATA_FRAMES, limits},
    environment::Trace,
    node::{
        Runtime, Settings,
        governor::{Governor, LaneLease},
    },
    test_support::{Fixture, connection},
};
use futures_util::FutureExt;
use std::{collections::VecDeque, time::Duration};
use tokio::sync::Notify;
use windshare_relay_core::{
    flow::Counts,
    identifiers::RelaySessionId,
    sessions::{Direction, OpaqueHeader, SessionKey},
    share::{Control, Input, Output, WorkResult},
};

struct Scenario {
    fixture: Fixture,
    relay: ShareRelay<Trace>,
    session: SessionKey,
    flow: FlowKey,
}

impl Scenario {
    fn new() -> Self {
        Self::with_incarnation(9)
    }

    fn with_incarnation(incarnation: u8) -> Self {
        let fixture = Fixture::new();
        let mut identity = fixture.identity();
        identity.incarnation =
            windshare_relay_core::lifecycle::Incarnation::new([incarnation; 16]).unwrap();
        let (mut relay, _) =
            ShareRelay::restore(identity, limits(), None, Trace, fixture.now(true)).unwrap();
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
        let step = relay.handle(
            Input::Control {
                connection: connection(1),
                frame: &fixture.bytes("descriptorUploadB64"),
            },
            at,
        );
        let work = step
            .outputs
            .into_iter()
            .find_map(|out| match out {
                Output::Work(work) => Some(work),
                _ => None,
            })
            .unwrap();
        let registered = relay.handle(
            Input::Completed {
                id: work.id,
                result: WorkResult::Complete,
            },
            at,
        );
        assert!(registered.outputs.iter().any(|output| matches!(
            output,
            Output::Send {
                control: Control::Registered(_),
                ..
            }
        )));
        let _ = relay.handle(Input::Connected(connection(3)), at);
        let step = relay.handle(
            Input::Control {
                connection: connection(3),
                frame: &fixture.bytes("joinB64"),
            },
            at,
        );
        let session = step
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
        let flow = step
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
        let step = self.relay.handle(
            Input::Grant {
                flow: self.flow,
                credit: Counts::new(frames, frames * u64::from(OpaqueHeader::MAX_ENCODED_BYTES)),
            },
            self.fixture.now(false),
        );
        assert_eq!(step.refusal, None);
    }

    fn receive(&mut self, payload: usize) -> (ReceivedFrame, Vec<u8>) {
        let mut bytes = b"WS2O\x02\0\0\0".to_vec();
        bytes.extend_from_slice(self.session.wire_id().as_bytes());
        bytes.extend_from_slice(&u32::try_from(payload).unwrap().to_be_bytes());
        bytes.resize(20 + payload, 7);
        let step = self.relay.handle(
            Input::Frame {
                source: connection(1),
                header: OpaqueHeader::decode(&bytes).unwrap(),
            },
            self.fixture.now(false),
        );
        assert_eq!(step.refusal, None);
        let receipt = step
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
        outbox
            .queue(receipt, bytes, Attribution::Anonymous)
            .unwrap();
    }

    fn submit(&mut self, outbox: &mut Outbox) -> Option<RelayStep> {
        outbox.submit_next(
            &mut self.relay,
            self.fixture.now(false),
            Attribution::Anonymous,
        )
    }

    fn stop(&mut self) -> RelayStep {
        self.relay.handle(
            Input::Control {
                connection: connection(2),
                frame: &self.fixture.bytes("stopProofB64"),
            },
            self.fixture.now(false),
        )
    }
}

#[test]
fn synchronous_bandwidth_grant_does_not_wake_the_share() {
    let mut scenario = Scenario::new();
    let (mut outbox, _writer, _) = channel(connection(3), Runtime::default());
    let wake = Arc::new(Notify::new());
    outbox.set_wake(Arc::clone(&wake));

    scenario.queue(&mut outbox, 1);
    assert_eq!(scenario.submit(&mut outbox).unwrap().refusal, None);
    assert!(wake.notified().now_or_never().is_none());
}

#[tokio::test(start_paused = true)]
async fn synchronous_bandwidth_grant_still_wakes_the_other_waiting_direction() {
    let scenario = Scenario::new();
    let reverse = scenario
        .relay
        .delivery_flows()
        .find(|flow| flow.direction() == Direction::ToSender)
        .unwrap();
    let max_frame = u64::from(OpaqueHeader::MAX_ENCODED_BYTES);
    let (_directory, governor) = paced_governor(&scenario, max_frame);
    let waiting = TestLane::new(&governor, scenario.flow);
    let active = TestLane::new(&governor, reverse);

    assert!(waiting.take(max_frame));
    let _ = waiting.wake.notified().now_or_never();
    let frame = max_frame / 2;
    assert!(!waiting.take(frame));
    tokio::time::advance(Duration::from_secs(1)).await;

    assert!(active.take(frame));
    assert!(waiting.wake.notified().now_or_never().is_some());
    assert!(waiting.take(frame));
    assert!(active.wake.notified().now_or_never().is_none());
}

#[tokio::test(start_paused = true)]
async fn paced_bandwidth_grant_wakes_the_waiter() {
    let scenario = Scenario::new();
    let frame = u64::from(OpaqueHeader::MAX_ENCODED_BYTES);
    let (_directory, governor) = paced_governor(&scenario, frame);
    let waiting = TestLane::new(&governor, scenario.flow);

    assert!(waiting.take(frame));
    let _ = waiting.wake.notified().now_or_never();
    assert!(!waiting.take(frame));

    tokio::select! {
        () = governor.run() => panic!("governor stopped with a waiting lane"),
        result = tokio::time::timeout(Duration::from_secs(2), waiting.wake.notified()) => result.unwrap(),
    }
    assert!(waiting.take(frame));
}

fn paced_governor(scenario: &Scenario, rate: u64) -> (tempfile::TempDir, Governor) {
    let directory = tempfile::tempdir().unwrap();
    let store = Database::open(
        &directory.path().join("node.sqlite"),
        scenario.flow.session().relay(),
    )
    .unwrap();
    let settings = Settings {
        ceiling: crate::node::Ceiling {
            bytes_per_second: Some(rate),
            ..Default::default()
        },
        ..Settings::default()
    };
    (directory, Runtime::new(settings, store).unwrap().governor)
}

struct TestLane {
    governor: Governor,
    lease: LaneLease,
    window: Arc<write_window::WriteWindow>,
    wake: Arc<Notify>,
}

impl TestLane {
    fn new(governor: &Governor, flow: FlowKey) -> Self {
        let window = Arc::default();
        let wake = Arc::default();
        let lease = governor
            .register(
                flow,
                Attribution::Anonymous,
                Arc::clone(&wake),
                Arc::clone(&window),
            )
            .unwrap();
        Self {
            governor: governor.clone(),
            lease,
            window,
            wake,
        }
    }

    fn take(&self, size: u64) -> bool {
        self.governor
            .take(
                &self.window,
                std::iter::once((&self.lease, std::iter::once(size))),
            )
            .is_some()
    }
}

#[tokio::test]
async fn pressure_is_bounded_directional_and_clears_after_submission() {
    let mut scenario = Scenario::new();
    let (mut outbox, _writer, _) = channel(connection(3), crate::node::Runtime::default());
    scenario.queue(&mut outbox, 1);
    let now = tokio::time::Instant::now();
    assert_eq!(outbox.capacity_updates(now), vec![]);
    let later = now + std::time::Duration::from_secs(2);
    assert_eq!(
        outbox.capacity_updates(later),
        vec![(scenario.flow, 15_000)]
    );
    assert_eq!(outbox.capacity_updates(later), vec![]);
    assert_eq!(
        outbox.capacity_updates(later + std::time::Duration::from_secs(5)),
        vec![(scenario.flow, 15_000)]
    );
    assert!(scenario.submit(&mut outbox).is_some());
    assert_eq!(
        outbox.capacity_updates(later + std::time::Duration::from_secs(6)),
        vec![(scenario.flow, 0)]
    );
}

#[tokio::test]
async fn submission_is_fifo_bounded_and_old_receipts_cannot_cross_stop() {
    let mut scenario = Scenario::new();
    scenario.grant(3);
    let (mut outbox, mut writer, _) = channel(connection(3), crate::node::Runtime::default());
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
    let (mut outbox, _writer, _) = channel(connection(3), crate::node::Runtime::default());
    outbox.pending_limit = Counts::new(
        CONNECTION_DATA_FRAMES as u64,
        8 * u64::from(OpaqueHeader::MAX_ENCODED_BYTES),
    );
    let payload = OpaqueHeader::MAX_ENCODED_BYTES as usize - 20;
    for _ in 0..8 {
        scenario.queue(&mut outbox, payload);
    }
    let (receipt, bytes) = scenario.receive(1);
    assert!(
        outbox
            .queue(receipt, bytes, Attribution::Anonymous)
            .is_err()
    );
    assert_eq!(
        outbox.pending_bytes as u64,
        outbox.pending_limit.encoded_bytes
    );
    assert!(outbox.pending.len() < CONNECTION_DATA_FRAMES);
    outbox.discard(scenario.flow);
    assert!(outbox.pending.is_empty());
    assert_eq!(outbox.pending_bytes, 0);
}

#[test]
fn queue_rejects_mismatched_payload_and_destination() {
    let mut scenario = Scenario::new();
    let (mut wrong, _writer, _) = channel(connection(4), crate::node::Runtime::default());
    let (receipt, bytes) = scenario.receive(1);
    assert!(wrong.queue(receipt, bytes, Attribution::Anonymous).is_err());
    let (mut outbox, _writer, _) = channel(connection(3), crate::node::Runtime::default());
    scenario.grant(1);
    let (receipt, _) = scenario.receive(1);
    assert!(
        outbox
            .queue(receipt, vec![1], Attribution::Anonymous)
            .is_err()
    );
}

#[tokio::test]
async fn control_and_capacity_notice_budgets_are_independent_of_each_other_and_probes() {
    let (outbox, mut writer, _) = channel(connection(3), crate::node::Runtime::default());
    let notice = CapacityWait {
        session: RelaySessionId::new(1_u64.to_be_bytes()).unwrap(),
        direction: Direction::ToReceiver,
        milliseconds: 1,
    };
    for _ in 0..CAPACITY_NOTICE_QUEUE_FRAMES {
        outbox.capacity_wait(notice).unwrap();
    }
    assert!(outbox.capacity_wait(notice).is_err());
    for _ in 0..CONTROL_QUEUE_FRAMES {
        outbox.control(vec![1]).unwrap();
    }
    assert!(outbox.control(vec![1]).is_err());
    assert!(outbox.control(vec![0; MAX_CONTROL_BYTES + 1]).is_err());
    outbox.probe(vec![1]).unwrap();
    outbox.probe(vec![2]).unwrap();
    assert_eq!(*outbox.probe.borrow(), Some(vec![2]));
    for _ in 0..CAPACITY_NOTICE_QUEUE_FRAMES {
        assert_eq!(writer.recv().await.unwrap(), notice.encode().unwrap());
    }
    assert!(outbox.control(vec![1]).is_err());
    outbox.capacity_wait(notice).unwrap();
    assert_eq!(writer.recv().await.unwrap(), [1]);
    outbox.control(vec![3]).unwrap();
}

#[test]
fn frame_limit_is_independent_of_byte_capacity() {
    let mut scenario = Scenario::new();
    scenario.grant(2);
    let (mut outbox, _writer, _) = channel(connection(3), crate::node::Runtime::default());
    outbox.pending_limit.frames = 2;
    scenario.queue(&mut outbox, 1);
    scenario.queue(&mut outbox, 1);
    let (receipt, bytes) = scenario.receive(1);
    assert!(
        outbox
            .queue(receipt, bytes, Attribution::Anonymous)
            .is_err()
    );
    assert_eq!(outbox.pending.len(), 2);
    assert!(outbox.pending_bytes < CONNECTION_DATA_BYTES);
}

#[test]
fn byte_reservation_failure_does_not_consume_a_frame_slot_or_commit() {
    let mut scenario = Scenario::new();
    let (mut outbox, _writer, _) = channel(connection(3), crate::node::Runtime::default());
    scenario.queue(&mut outbox, 1);
    let held = Arc::clone(&outbox.pending.window.bytes)
        .try_acquire_many_owned(u32::try_from(crate::config::SUBMITTED_DATA_BYTES - 20).unwrap())
        .unwrap();
    assert!(scenario.submit(&mut outbox).is_none());
    assert_eq!(outbox.pending.len(), 1);
    assert_eq!(
        outbox.pending.window.frames.available_permits(),
        SUBMITTED_DATA_FRAMES
    );
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
#[path = "fairness_tests.rs"]
mod fairness;
