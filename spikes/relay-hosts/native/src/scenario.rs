//! Authenticated canonical fixtures isolate host behavior from unfinished client cutover.
//! Storage is completed in memory: this probe makes no durability or full-product claim.
use base64::{Engine, engine::general_purpose::STANDARD};
use std::{collections::VecDeque, num::NonZeroU64, time::Duration};
use windshare_relay_core::{
    challenge::ChallengeSeed,
    clock::UnixMillis,
    flow::{Counts, FlowKey, ReceivedFrame},
    lifecycle::{ConnectionRef, Incarnation, Record},
    observation::{Observation, Observer},
    registration::RegisterInit,
    sessions::{OpaqueHeader, SessionAdmitted},
    share::{
        AttemptRate, Control, HandshakeLimits, Identity, Input, Limits, Output, Responsibility,
        Restore, ShareRelay, Submission, Turn, WorkResult,
    },
};

pub const FRAME_BYTES: usize = OpaqueHeader::MAX_ENCODED_BYTES as usize;
pub const WINDOW: usize = 8;
const CONNECTION_CAPACITY: u64 = 16;
const CONNECTION_LIMIT: usize = 32;
const HANDSHAKE_LIMIT: usize = 8;
// Frozen fixture time must allow setup and replacement attempts without replenishment.
const ATTEMPT_BURST: u32 = 64;
const ATTEMPT_REFILL: Duration = Duration::from_millis(100);
const SESSION_LIMIT: usize = 24;
const STARTUP_RESERVE: u64 = 2;
const REGISTRATION_BUDGET: u64 = 5_000;
const STOP_BUDGET: u64 = 10_000;
const SETTLEMENT_BUDGET: u64 = 5_000;
const STOP_CONNECTION: u64 = 99;

#[derive(Default, Debug)]
pub struct Trace {
    pub events: Vec<Observation>,
}
impl Observer for Trace {
    fn observe(&mut self, value: Observation) {
        // Per-frame success is already silent in the kernel. Bound rare failure evidence too.
        if self.events.len() < 4096 {
            self.events.push(value);
        }
    }
}

pub fn connection(id: u64) -> ConnectionRef {
    ConnectionRef {
        id: NonZeroU64::new(id).unwrap(),
        generation: NonZeroU64::new(1).unwrap(),
    }
}

pub struct Scenario {
    pub relay: ShareRelay<Trace>,
    pub now: UnixMillis,
    data: serde_json::Value,
    pub close_requests: Vec<ConnectionRef>,
    pub discard_requests: Vec<FlowKey>,
    pub stop_acknowledged: bool,
    pub stored: Option<Record>,
}
impl Scenario {
    pub fn new() -> Self {
        let vectors: serde_json::Value =
            serde_json::from_str(include_str!("../../../../core/testvectors/v2-session.json"))
                .unwrap();
        let data = vectors["cases"]
            .as_array()
            .unwrap()
            .iter()
            .find(|case| case["name"] == "relay-capacity-registration")
            .unwrap()
            .clone();
        let text = |key: &str| data[key].as_str().unwrap().to_owned();
        let frame = |key: &str| STANDARD.decode(text(key)).unwrap();
        let now =
            UnixMillis(text("expiresAt").parse::<u64>().unwrap() * 1000 - REGISTRATION_BUDGET - 1);
        let init = RegisterInit::decode(&frame("registerInitB64")).unwrap();
        let capacity = |frames| Counts::new(frames, frames * FRAME_BYTES as u64);
        let limits = Limits {
            business_connections: CONNECTION_LIMIT,
            handshakes: HandshakeLimits {
                concurrent: HANDSHAKE_LIMIT,
                attempts: AttemptRate {
                    burst: ATTEMPT_BURST,
                    refill: ATTEMPT_REFILL,
                },
            },
            sessions: SESSION_LIMIT,
            retained_flows_per_connection: SESSION_LIMIT,
            retirement: windshare_relay_core::share::RetirementPolicy::PhysicalRelease,
            delivery: windshare_relay_core::flow::Limits::new(
                capacity(CONNECTION_CAPACITY),
                capacity(STARTUP_RESERVE),
                capacity(1),
                Duration::from_millis(SETTLEMENT_BUDGET),
            )
            .unwrap(),
        };
        let identity = Identity {
            relay: windshare_relay_core::identifiers::RelayIdentity::new(
                frame("relayIdentityB64").try_into().unwrap(),
            )
            .unwrap(),
            share: init.registration().route().share_id(),
            incarnation: Incarnation::new([1; 16]).unwrap(),
        };
        let (relay, initial) = ShareRelay::restore(
            identity,
            limits,
            &Restore::restarted(None, Responsibility::Released { previous: None }),
            Trace::default(),
            now,
        )
        .unwrap();
        assert!(initial.outputs.is_empty());
        let mut scenario = Self {
            relay,
            now,
            data,
            close_requests: vec![],
            discard_requests: vec![],
            stop_acknowledged: false,
            stored: None,
        };
        // Issue STOP's independently timed challenge before registration, as in core scenarios.
        let stop_at = UnixMillis(
            scenario.text("stopExpiresAt").parse::<u64>().unwrap() * 1000 - STOP_BUDGET - 1,
        );
        scenario.handle_at(Input::Connected(connection(STOP_CONNECTION)), stop_at);
        scenario.challenge(
            STOP_CONNECTION,
            "stopInitB64",
            "stopChallengeIdB64",
            "stopChallengeNonceB64",
            stop_at,
        );
        scenario.handle(Input::Connected(connection(1)));
        scenario.challenge(
            1,
            "registerInitB64",
            "challengeIdB64",
            "challengeNonceB64",
            now,
        );
        scenario.control(1, "registerProofB64");
        scenario.control(1, "descriptorUploadB64");
        scenario
    }
    fn text(&self, field: &str) -> &str {
        self.data[field].as_str().unwrap()
    }
    fn bytes(&self, field: &str) -> Vec<u8> {
        STANDARD.decode(self.text(field)).unwrap()
    }
    fn challenge(&mut self, id: u64, init: &str, seed_id: &str, nonce: &str, at: UnixMillis) {
        let frame = self.bytes(init);
        let turn = self.relay.handle(
            Input::Control {
                connection: connection(id),
                frame: &frame,
            },
            at,
        );
        assert!(turn.refusal.is_none(), "{:?}", turn.refusal);
        let request = turn
            .outputs
            .into_iter()
            .find_map(|output| match output {
                Output::ChallengeNeeded(request) => Some(request),
                _ => None,
            })
            .unwrap();
        let seed = ChallengeSeed::new(
            self.bytes(seed_id).try_into().unwrap(),
            self.bytes(nonce).try_into().unwrap(),
        )
        .unwrap();
        self.handle_at(Input::Challenge { request, seed }, at);
    }
    fn control(&mut self, id: u64, field: &str) {
        let frame = self.bytes(field);
        self.handle(Input::Control {
            connection: connection(id),
            frame: &frame,
        });
    }
    pub fn handle(&mut self, input: Input<'_>) {
        self.handle_at(input, self.now);
    }
    fn handle_at(&mut self, input: Input<'_>, at: UnixMillis) {
        let turn = self.relay.handle(input, at);
        self.outputs(turn);
    }
    pub fn outputs(&mut self, turn: Turn) {
        assert!(turn.refusal.is_none(), "{:?}", turn.refusal);
        let mut outputs: VecDeque<_> = turn.outputs.into();
        while let Some(output) = outputs.pop_front() {
            let follow = match output {
                Output::Work(work) => {
                    use windshare_relay_core::share::Storage;
                    match work.operation {
                        Storage::Register { ownership, .. } => {
                            assert!(self.stored.is_none());
                            self.stored = Some(Record::Published(*ownership));
                        }
                        Storage::CompareExchange { expected, next } => {
                            assert_eq!(self.stored, expected.map(|r| *r));
                            self.stored = next.map(|r| *r);
                        }
                    }
                    Some(self.relay.handle(
                        Input::Completed {
                            id: work.id,
                            result: WorkResult::Complete,
                        },
                        self.now,
                    ))
                }
                Output::Close(value) => {
                    self.close_requests.push(value);
                    None
                }
                Output::Discard(flow) => {
                    self.discard_requests.push(flow);
                    None
                }
                Output::Send {
                    control: Control::Stopped(_),
                    ..
                } => {
                    self.stop_acknowledged = true;
                    None
                }
                Output::Send { .. } => None,
                other => panic!("unhandled host obligation: {other:?}"),
            };
            if let Some(next) = follow {
                assert!(next.refusal.is_none(), "{:?}", next.refusal);
                outputs.extend(next.outputs);
            }
        }
    }
    pub fn join(&mut self, id: u64) -> FlowKey {
        self.handle(Input::Connected(connection(id)));
        let frame = self.bytes("joinB64");
        let turn = self.relay.handle(
            Input::Control {
                connection: connection(id),
                frame: &frame,
            },
            self.now,
        );
        assert!(turn.refusal.is_none());
        let flow = turn
            .outputs
            .iter()
            .find_map(|output| match output {
                Output::Send {
                    control: Control::Grant(grant),
                    ..
                } if grant.flow.destination() == connection(id) => Some(grant.flow),
                _ => None,
            })
            .expect("receiver admission");
        self.outputs(turn);
        // Establish the receiver-first handshake in the fixture before measuring active traffic.
        let bootstrap = payload(flow, 0);
        let receipt = self.receive_from(connection(id), &bootstrap);
        self.submit(&receipt, |_| Submission::Complete);
        drop(bootstrap);
        self.handle(Input::Consumed {
            connection: connection(1),
            session: flow.session().wire_id(),
            total: Counts::new(1, FRAME_BYTES as u64),
        });
        let mut admitted = b"WS2M\x02\0\0\0".to_vec();
        admitted.extend_from_slice(flow.session().wire_id().as_bytes());
        self.handle(Input::Admitted {
            sender: connection(1),
            frame: SessionAdmitted::decode(&admitted).unwrap(),
        });
        self.handle(Input::Grant {
            flow,
            credit: Counts::new((WINDOW - 1) as u64, ((WINDOW - 1) * FRAME_BYTES) as u64),
        });
        flow
    }
    pub fn receive(&mut self, bytes: &[u8]) -> ReceivedFrame {
        self.receive_from(connection(1), bytes)
    }
    fn receive_from(&mut self, source: ConnectionRef, bytes: &[u8]) -> ReceivedFrame {
        let header = OpaqueHeader::decode(bytes).unwrap();
        let turn = self.relay.handle(Input::Frame { source, header }, self.now);
        assert!(turn.refusal.is_none(), "{:?}", turn.refusal);
        let mut receipt = None;
        let mut remaining = Turn::default();
        for output in turn.outputs {
            match output {
                Output::Received(value) => receipt = Some(value),
                value => remaining.outputs.push(value),
            }
        }
        self.outputs(remaining);
        receipt.unwrap()
    }
    pub fn submit(&mut self, receipt: &ReceivedFrame, handoff: impl FnOnce(FlowKey) -> Submission) {
        let turn = self.relay.submit(receipt, self.now, handoff);
        self.outputs(turn);
    }
    pub fn consume(&mut self, flow: FlowKey, total: Counts, delta: Counts) {
        self.handle(Input::Consumed {
            connection: flow.destination(),
            session: flow.session().wire_id(),
            total,
        });
        self.handle(Input::Grant {
            flow,
            credit: delta,
        });
    }
    pub fn stop(&mut self) {
        self.control(STOP_CONNECTION, "stopProofB64");
    }
    pub fn closed(&mut self, id: u64) {
        self.handle(Input::Closed(connection(id)));
    }
}

pub fn payload(flow: FlowKey, ordinal: u64) -> Vec<u8> {
    let mut bytes = vec![0x5a; FRAME_BYTES];
    bytes[..8].copy_from_slice(b"WS2O\x02\0\0\0");
    bytes[8..16].copy_from_slice(flow.session().wire_id().as_bytes());
    bytes[16..20].copy_from_slice(&((FRAME_BYTES - 20) as u32).to_be_bytes());
    bytes[20..28].copy_from_slice(&ordinal.to_be_bytes());
    bytes
}
