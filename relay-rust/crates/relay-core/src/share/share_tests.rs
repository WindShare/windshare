extern crate std;
use super::*;
use crate::{
    clock::UnixMillis,
    flow::{Counts, FlowKey, ReceivedFrame},
    lifecycle::Incarnation,
    observation::{Observation, Observer},
    registration::RegisterInit,
    sessions::{OpaqueHeader, SessionAdmitted, SessionKey},
    test_vectors::{bytes, challenge_seed, relay_identity, session_case, unix_seconds},
    wire::Writer,
};
use core::{num::NonZeroU64, time::Duration};
use std::vec::Vec;

const SESSION_LIMIT: usize = 8;
const REGISTRATION_BUDGET: u64 = 5_000;
const STOP_BUDGET: u64 = 10_000;
const SETTLEMENT_BUDGET: u64 = 5_000;

#[derive(Default, Debug)]
struct Trace(Vec<Observation>);
impl Observer for Trace {
    fn observe(&mut self, observation: Observation) {
        self.0.push(observation);
    }
}
type Relay = ShareRelay<Trace>;

struct Fixture {
    data: serde_json::Value,
    at: UnixMillis,
}
impl Fixture {
    fn new() -> Self {
        let data: serde_json::Value = session_case("relay-capacity-registration");
        let at = UnixMillis(
            unix_seconds(data["expiresAt"].as_str().unwrap()).start().0 - REGISTRATION_BUDGET - 1,
        );
        Self { data, at }
    }
    fn text(&self, name: &str) -> &str {
        self.data[name].as_str().unwrap()
    }
    fn frame(&self, name: &str) -> Vec<u8> {
        bytes(self.text(name))
    }
    fn identity(&self, incarnation: u8) -> Identity {
        let init = RegisterInit::decode(&self.frame("registerInitB64")).unwrap();
        Identity {
            relay: relay_identity(self.text("relayIdentityB64")),
            share: init.registration().route().share_id(),
            incarnation: Incarnation::new([incarnation; 16]).unwrap(),
        }
    }
    fn limits() -> Limits {
        let capacity =
            |frames| Counts::new(frames, frames * u64::from(OpaqueHeader::MAX_ENCODED_BYTES));
        Limits {
            connections: 16,
            sessions: SESSION_LIMIT,
            delivery: crate::flow::Limits::new(
                capacity(16),
                capacity(8),
                capacity(1),
                Duration::from_millis(SETTLEMENT_BUDGET),
            )
            .unwrap(),
        }
    }
    fn relay(&self) -> Relay {
        let (relay, turn) = Relay::restore(
            self.identity(1),
            Self::limits(),
            &Restore::restarted(None),
            Trace::default(),
            self.at,
        )
        .unwrap();
        assert!(turn.outputs.is_empty());
        relay
    }
    fn control(
        &self,
        relay: &mut Relay,
        connection: ConnectionRef,
        field: &str,
        at: UnixMillis,
    ) -> Turn {
        relay.handle(
            Input::Control {
                connection,
                frame: &self.frame(field),
            },
            at,
        )
    }
    fn begin_registration(&self, relay: &mut Relay) -> Work {
        let sender = connection(1);
        assert_eq!(
            relay.handle(Input::Connected(sender), self.at).refusal,
            None
        );
        let turn = self.control(relay, sender, "registerInitB64", self.at);
        assert!(
            matches!(turn.outputs.as_slice(), [Output::ChallengeNeeded(value)] if value.connection == sender)
        );
        let seed = challenge_seed(self.text("challengeIdB64"), self.text("challengeNonceB64"));
        assert_eq!(
            relay
                .handle(
                    Input::Challenge {
                        request: challenge(&turn),
                        seed
                    },
                    self.at
                )
                .refusal,
            None
        );
        assert!(
            self.control(relay, sender, "registerProofB64", self.at)
                .outputs
                .is_empty()
        );
        work(&self.control(relay, sender, "descriptorUploadB64", self.at))
    }
    fn register(&self, relay: &mut Relay) -> Record {
        let publish = self.begin_registration(relay);
        let durable = work(&complete(relay, &publish, self.at));
        assert!(matches!(durable.operation, Storage::CompareExchange { .. }));
        let turn = complete(relay, &durable, self.at);
        assert!(turn.outputs.iter().any(|output| matches!(
            output,
            Output::Send {
                control: Control::Registered(_),
                ..
            }
        )));
        next_record(&durable)
    }
    fn join(&self, relay: &mut Relay, receiver: u64) -> (SessionKey, [FlowKey; 2]) {
        assert_eq!(
            relay
                .handle(Input::Connected(connection(receiver)), self.at)
                .refusal,
            None
        );
        let turn = self.control(relay, connection(receiver), "joinB64", self.at);
        let key = turn
            .outputs
            .iter()
            .find_map(|output| match output {
                Output::Send {
                    control: Control::Descriptor { session, .. },
                    ..
                } => Some(*session),
                _ => None,
            })
            .expect("receiver descriptor");
        let flows: Vec<_> = turn
            .outputs
            .iter()
            .filter_map(|output| match output {
                Output::Send {
                    control: Control::Grant(grant),
                    ..
                } => Some(grant.flow),
                _ => None,
            })
            .collect();
        assert_eq!(flows.len(), 2);
        (key, [flows[0], flows[1]])
    }
    fn prepare_stop(&self, relay: &mut Relay) {
        let at = UnixMillis(unix_seconds(self.text("stopExpiresAt")).start().0 - STOP_BUDGET - 1);
        assert_eq!(
            relay.handle(Input::Connected(connection(99)), at).refusal,
            None
        );
        let turn = self.control(relay, connection(99), "stopInitB64", at);
        assert!(matches!(
            turn.outputs.as_slice(),
            [Output::ChallengeNeeded(_)]
        ));
        let seed = challenge_seed(
            self.text("stopChallengeIdB64"),
            self.text("stopChallengeNonceB64"),
        );
        assert_eq!(
            relay
                .handle(
                    Input::Challenge {
                        request: challenge(&turn),
                        seed
                    },
                    at
                )
                .refusal,
            None
        );
    }
    fn begin_resume(&self, relay: &mut Relay, slot: u64) -> UnixMillis {
        let at = UnixMillis(
            unix_seconds(self.text("resumeExpiresAt")).start().0 - REGISTRATION_BUDGET - 1,
        );
        let _ = relay.handle(Input::Connected(connection(slot)), at);
        let _ = self.control(relay, connection(slot), "resumeInitB64", at);
        let requested = self.control(relay, connection(slot), "resumeCredentialB64", at);
        let seed = challenge_seed(
            self.text("resumeChallengeIdB64"),
            self.text("resumeChallengeNonceB64"),
        );
        assert_eq!(
            relay
                .handle(
                    Input::Challenge {
                        request: challenge(&requested),
                        seed
                    },
                    at
                )
                .refusal,
            None
        );
        at
    }
    fn stop(&self, relay: &mut Relay) -> Turn {
        self.control(relay, connection(99), "stopProofB64", self.at)
    }
}

fn connection(id: u64) -> ConnectionRef {
    ConnectionRef {
        id: NonZeroU64::new(id).unwrap(),
        generation: NonZeroU64::new(1).unwrap(),
    }
}
fn challenge(turn: &Turn) -> ChallengeRequest {
    turn.outputs
        .iter()
        .find_map(|output| match output {
            Output::ChallengeNeeded(request) => Some(*request),
            _ => None,
        })
        .expect("challenge requested")
}
fn work(turn: &Turn) -> Work {
    turn.outputs
        .iter()
        .find_map(|output| match output {
            Output::Work(work) => Some(work.clone()),
            _ => None,
        })
        .unwrap_or_else(|| panic!("expected work: {turn:?}"))
}
fn complete(relay: &mut Relay, work: &Work, at: UnixMillis) -> Turn {
    relay.handle(
        Input::Completed {
            id: work.id,
            result: WorkResult::Complete,
        },
        at,
    )
}
fn next_record(work: &Work) -> Record {
    match &work.operation {
        Storage::CompareExchange {
            next: Some(record), ..
        }
        | Storage::Publish { record, .. } => **record,
        _ => panic!("expected stored record"),
    }
}
fn receive(relay: &mut Relay, key: SessionKey, at: UnixMillis) -> ReceivedFrame {
    let mut writer = Writer::<21>::with_header(*b"WS2O", 0);
    writer
        .put(key.wire_id().as_bytes())
        .put(&1_u32.to_be_bytes())
        .put(&[1]);
    let header = OpaqueHeader::decode(&writer.into_frame()).unwrap();
    let turn = relay.handle(
        Input::Frame {
            source: key.receiver(),
            header,
        },
        at,
    );
    turn.outputs
        .into_iter()
        .find_map(|output| match output {
            Output::Received(receipt) => Some(receipt),
            _ => None,
        })
        .expect("accepted payload receipt")
}
fn activate(relay: &mut Relay, key: SessionKey, at: UnixMillis) {
    let mut writer = Writer::<16>::with_header(*b"WS2M", 0);
    writer.put(key.wire_id().as_bytes());
    let frame = SessionAdmitted::decode(&writer.into_frame()).unwrap();
    assert_eq!(
        relay
            .handle(
                Input::Admitted {
                    sender: key.sender(),
                    frame
                },
                at
            )
            .refusal,
        None
    );
}

#[test]
fn publication_waits_for_final_durability_and_retries_exact_work() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let publication = fixture.begin_registration(&mut relay);
    let durable = work(&complete(&mut relay, &publication, fixture.at));
    assert_eq!(
        relay
            .handle(Input::Connected(connection(2)), fixture.at)
            .refusal,
        None
    );
    let join = fixture.control(&mut relay, connection(2), "joinB64", fixture.at);
    assert!(!join.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Descriptor { .. },
            ..
        }
    )));
    let retry = relay.handle(
        Input::Completed {
            id: durable.id,
            result: WorkResult::Uncertain,
        },
        fixture.at,
    );
    assert!(matches!(retry.outputs.as_slice(), [Output::Retry(id)] if *id == durable.id));
    let resent = work(&relay.handle(Input::Retry(durable.id), fixture.at));
    assert_eq!(resent, durable);
    assert!(
        complete(&mut relay, &durable, fixture.at)
            .outputs
            .iter()
            .any(|output| matches!(
                output,
                Output::Send {
                    control: Control::Registered(_),
                    ..
                }
            ))
    );
}

#[test]
fn stop_revokes_pending_receipts_and_acknowledges_only_after_physical_cleanup() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.prepare_stop(&mut relay);
    fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    let receipt = receive(&mut relay, key, fixture.at);
    let stop = fixture.stop(&mut relay);
    assert!(
        !stop
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Discard(_) | Output::Close(_)))
    );
    let intent = work(&stop);
    let mut called = false;
    let refused = relay.submit(&receipt, fixture.at, |_| {
        called = true;
        Submission::Complete
    });
    assert!(refused.refusal.is_some());
    assert!(!called);
    let cleanup = complete(&mut relay, &intent, fixture.at);
    assert!(
        cleanup
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Discard(_)))
    );
    assert!(!cleanup.outputs.iter().any(|output| matches!(
        output,
        Output::Work(_)
            | Output::Send {
                control: Control::Stopped(_),
                ..
            }
    )));
    let first = relay.handle(Input::Closed(key.receiver()), fixture.at);
    assert!(
        !first
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Work(_)))
    );
    let committed = work(&relay.handle(Input::Closed(key.sender()), fixture.at));
    let acknowledged = complete(&mut relay, &committed, fixture.at);
    assert!(acknowledged.outputs.iter().any(|output| matches!(output, Output::Send { connection: target, control: Control::Stopped(_) } if *target == connection(99))));
    assert!(
        relay
            .handle(
                Input::Completed {
                    id: intent.id,
                    result: WorkResult::Complete
                },
                fixture.at
            )
            .outputs
            .is_empty()
    );
}

#[test]
fn cancellation_keeps_submitted_occupancy_and_does_not_close_a_shared_sender() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (first, flows) = fixture.join(&mut relay, 2);
    let (sibling, _) = fixture.join(&mut relay, 3);
    let receipt = receive(&mut relay, first, fixture.at);
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    let cancelled = relay.handle(
        Input::Cancel {
            participant: first.receiver(),
            session: first.wire_id(),
        },
        fixture.at,
    );
    assert_eq!(
        cancelled
            .outputs
            .iter()
            .filter(|output| matches!(output, Output::Discard(_)))
            .count(),
        2
    );
    assert!(
        !cancelled
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(target) if *target == first.sender()))
    );
    let early = relay.handle(
        Input::Settled {
            connection: first.sender(),
            session: flows[0].session().wire_id(),
            total: Counts::new(1, 21),
        },
        fixture.at,
    );
    assert_eq!(
        early.refusal,
        Some(Error::Flow(crate::flow::Refusal::CleanupPending))
    );
    for flow in flows {
        assert_eq!(
            relay.handle(Input::Discarded(flow), fixture.at).refusal,
            None
        );
    }
    let settled = relay.handle(
        Input::Settled {
            connection: first.sender(),
            session: flows[0].session().wire_id(),
            total: Counts::new(1, 21),
        },
        fixture.at,
    );
    assert_eq!(settled.refusal, None);
    assert!(
        settled
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(target) if *target == first.receiver()))
    );
    assert!(
        !settled
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(target) if *target == first.sender()))
    );
    let receipt = receive(&mut relay, sibling, fixture.at);
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
}

#[test]
fn merged_deadline_expires_before_a_refused_input_without_losing_cleanup() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    let deadline = relay.deadline().unwrap();
    let turn = relay.handle(
        Input::Cancel {
            participant: connection(7),
            session: key.wire_id(),
        },
        deadline,
    );
    assert!(turn.refusal.is_some());
    assert!(
        turn.outputs
            .iter()
            .any(|output| matches!(output, Output::Discard(_)))
    );
    let later = relay.handle(Input::Tick, UnixMillis(deadline.0 + SETTLEMENT_BUDGET));
    assert!(
        later
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(_)))
    );
}

#[test]
fn fifo_refuses_out_of_order_handoff_and_uncertain_handoff_never_refunds() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    assert_eq!(
        relay
            .handle(
                Input::Grant {
                    flow: flows[0],
                    credit: Counts::new(1, 21)
                },
                fixture.at
            )
            .refusal,
        None
    );
    let first = receive(&mut relay, key, fixture.at);
    let second = receive(&mut relay, key, fixture.at);
    let refused = relay.submit(&second, fixture.at, |_| panic!("out of order callback"));
    assert_eq!(
        refused.refusal,
        Some(Error::Flow(crate::flow::Refusal::SubmissionOrder))
    );
    assert_eq!(
        relay
            .submit(&first, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    let failed = relay.submit(&second, fixture.at, |_| Submission::Uncertain);
    assert!(
        failed
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == key.sender()))
    );
}

#[test]
fn native_actor_can_move_between_executor_threads() {
    fn assert_send<T: Send>() {}
    assert_send::<Relay>();
}

#[path = "ordering_tests.rs"]
mod ordering;

#[path = "ownership_tests.rs"]
mod ownership;

#[path = "handshake_restore_tests.rs"]
mod handshakes;

#[path = "recovery_tests.rs"]
mod recovery;

#[path = "capacity_tests.rs"]
mod capacity;

#[path = "metadata_tests.rs"]
mod metadata;

#[path = "checkpoint_tests.rs"]
mod checkpoints;

#[path = "settlement_tests.rs"]
mod settlement;
