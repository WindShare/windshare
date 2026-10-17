use crate::{config::limits, environment::Trace};
use base64::{Engine, engine::general_purpose::STANDARD};
use std::num::NonZeroU64;
use windshare_relay_core::{
    challenge::ChallengeSeed,
    clock::UnixMillis,
    identifiers::RelayIdentity,
    lifecycle::{ConnectionRef, Incarnation},
    registration::RegisterInit,
    share::{
        Identity, Input, Output, Responsibility, Restore, ShareRelay, Storage, Turn, Work,
        WorkResult,
    },
};

pub struct Fixture(pub serde_json::Value);

impl Fixture {
    pub fn new() -> Self {
        let vectors: serde_json::Value =
            serde_json::from_str(include_str!("../../../../core/testvectors/v2-session.json"))
                .unwrap();
        Self(
            vectors["cases"]
                .as_array()
                .unwrap()
                .iter()
                .find(|case| case["name"] == "relay-capacity-registration")
                .unwrap()
                .clone(),
        )
    }
    pub fn bytes(&self, key: &str) -> Vec<u8> {
        STANDARD.decode(self.0[key].as_str().unwrap()).unwrap()
    }
    pub fn now(&self, stop: bool) -> UnixMillis {
        let field = if stop { "stopExpiresAt" } else { "expiresAt" };
        let budget = if stop { 10_000 } else { 5_000 };
        UnixMillis(self.0[field].as_str().unwrap().parse::<u64>().unwrap() * 1000 - budget - 1)
    }
    pub fn seed(&self, stop: bool) -> ChallengeSeed {
        let fields = if stop {
            ["stopChallengeIdB64", "stopChallengeNonceB64"]
        } else {
            ["challengeIdB64", "challengeNonceB64"]
        };
        ChallengeSeed::new(
            self.bytes(fields[0]).try_into().unwrap(),
            self.bytes(fields[1]).try_into().unwrap(),
        )
        .unwrap()
    }
    pub fn identity(&self) -> Identity {
        let init = RegisterInit::decode(&self.bytes("registerInitB64")).unwrap();
        Identity {
            relay: RelayIdentity::new(self.bytes("relayIdentityB64").try_into().unwrap()).unwrap(),
            share: init.registration().route().share_id(),
            incarnation: Incarnation::new([9; 16]).unwrap(),
        }
    }
    pub fn works(&self) -> Vec<Storage> {
        let (mut relay, _) = ShareRelay::restore(
            self.identity(),
            limits(),
            &Restore::restarted(None, Responsibility::Released),
            Trace,
            self.now(true),
        )
        .unwrap();
        self.begin(&mut relay, true);
        self.begin(&mut relay, false);
        let _ = relay.handle(
            Input::Control {
                connection: connection(1),
                frame: &self.bytes("registerProofB64"),
            },
            self.now(false),
        );
        let publication = work(relay.handle(
            Input::Control {
                connection: connection(1),
                frame: &self.bytes("descriptorUploadB64"),
            },
            self.now(false),
        ));
        let ownership = work(relay.handle(
            Input::Completed {
                id: publication.id,
                result: WorkResult::Complete,
            },
            self.now(false),
        ));
        let _ = relay.handle(
            Input::Completed {
                id: ownership.id,
                result: WorkResult::Complete,
            },
            self.now(false),
        );
        let stopping = work(relay.handle(
            Input::Control {
                connection: connection(2),
                frame: &self.bytes("stopProofB64"),
            },
            self.now(false),
        ));
        let stopped = work(relay.handle(
            Input::Completed {
                id: stopping.id,
                result: WorkResult::Complete,
            },
            self.now(false),
        ));
        vec![
            publication.operation,
            ownership.operation,
            stopping.operation,
            stopped.operation,
        ]
    }
    fn begin(&self, relay: &mut ShareRelay<Trace>, stop: bool) {
        let connection = connection(if stop { 2 } else { 1 });
        let now = self.now(stop);
        let _ = relay.handle(Input::Connected(connection), now);
        let init = self.bytes(if stop {
            "stopInitB64"
        } else {
            "registerInitB64"
        });
        let turn = relay.handle(
            Input::Control {
                connection,
                frame: &init,
            },
            now,
        );
        let request = turn
            .outputs
            .into_iter()
            .find_map(|output| match output {
                Output::ChallengeNeeded(request) => Some(request),
                _ => None,
            })
            .unwrap();
        let _ = relay.handle(
            Input::Challenge {
                request,
                seed: self.seed(stop),
            },
            now,
        );
    }
}

fn work(turn: Turn) -> Work {
    assert!(turn.refusal.is_none(), "{:?}", turn.refusal);
    turn.outputs
        .into_iter()
        .find_map(|output| match output {
            Output::Work(work) => Some(work),
            _ => None,
        })
        .unwrap()
}

pub fn connection(id: u64) -> ConnectionRef {
    ConnectionRef {
        id: NonZeroU64::new(id).unwrap(),
        generation: NonZeroU64::MIN,
    }
}
