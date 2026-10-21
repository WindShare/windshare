use super::{
    config,
    runtime::{Host, Recovery, Runtime},
};
use anyhow::{Result, bail};
use base64::{Engine, engine::general_purpose::STANDARD};
use std::{
    collections::{BTreeMap, BTreeSet},
    num::NonZeroU64,
};
use windshare_relay_core::{
    authority::{self, StoredAuthority},
    challenge::ChallengeSeed,
    clock::UnixMillis,
    identifiers::RelayIdentity,
    lifecycle::{ConnectionRef, Incarnation, Record},
    registration::RegisterInit,
    share::{
        Checkpoint, CheckpointManifest, CheckpointTarget, ConnectionCheckpoint, Identity, Input,
        Storage,
    },
};

#[path = "access_tests.rs"]
mod access;

#[path = "recovery_tests.rs"]
mod recovery;

#[path = "delivery_tests.rs"]
mod delivery;

struct Fixture(serde_json::Value);
impl Fixture {
    fn new() -> Self {
        let vectors: serde_json::Value =
            serde_json::from_str(include_str!("../../../../core/testvectors/v2-session.json"))
                .unwrap();
        Self(
            vectors["cases"]
                .as_array()
                .unwrap()
                .iter()
                .find(|v| v["name"] == "relay-capacity-registration")
                .unwrap()
                .clone(),
        )
    }
    fn frame(&self, key: &str) -> Vec<u8> {
        STANDARD.decode(self.0[key].as_str().unwrap()).unwrap()
    }
    fn time(&self, prefix: &str) -> UnixMillis {
        let key = if prefix.is_empty() {
            "expiresAt"
        } else if prefix == "stop" {
            "stopExpiresAt"
        } else {
            "resumeExpiresAt"
        };
        let budget = if prefix == "stop" { 10_000 } else { 5_000 };
        UnixMillis(self.0[key].as_str().unwrap().parse::<u64>().unwrap() * 1000 - budget - 1)
    }
    fn seed(&self, prefix: &str) -> ChallengeSeed {
        let id = if prefix.is_empty() {
            "challengeIdB64".to_owned()
        } else {
            format!("{prefix}ChallengeIdB64")
        };
        let nonce = if prefix.is_empty() {
            "challengeNonceB64".to_owned()
        } else {
            format!("{prefix}ChallengeNonceB64")
        };
        ChallengeSeed::new(
            self.frame(&id).try_into().unwrap(),
            self.frame(&nonce).try_into().unwrap(),
        )
        .unwrap()
    }
    fn identity(&self, boot: u8) -> Identity {
        Identity {
            relay: RelayIdentity::new(self.frame("relayIdentityB64").try_into().unwrap()).unwrap(),
            share: RegisterInit::decode(&self.frame("registerInitB64"))
                .unwrap()
                .registration()
                .route()
                .share_id(),
            incarnation: Incarnation::new([boot; 16]).unwrap(),
        }
    }
}
fn connection(id: u64) -> ConnectionRef {
    ConnectionRef {
        id: NonZeroU64::new(id).unwrap(),
        generation: NonZeroU64::MIN,
    }
}

struct Memory {
    initialized: bool,
    identity: Identity,
    time: UnixMillis,
    seed: Option<ChallengeSeed>,
    stored: Option<StoredAuthority>,
    manifest: Option<Vec<u8>>,
    attachments: BTreeMap<ConnectionRef, Vec<u8>>,
    live: BTreeSet<ConnectionRef>,
    sent: Vec<(ConnectionRef, Vec<u8>)>,
    writes: usize,
    fail_write: Option<(usize, bool)>,
    failed_attachments: usize,
    manifest_writes: usize,
    fail_send: Option<ConnectionRef>,
}
impl Memory {
    fn new(f: &Fixture) -> Self {
        Self {
            initialized: false,
            identity: f.identity(1),
            time: f.time(""),
            seed: Some(f.seed("")),
            stored: None,
            manifest: None,
            attachments: BTreeMap::new(),
            live: BTreeSet::new(),
            sent: Vec::new(),
            writes: 0,
            fail_write: None,
            failed_attachments: 0,
            manifest_writes: 0,
            fail_send: None,
        }
    }
    fn restore(&mut self, boot: u8) -> Result<Runtime<()>> {
        self.identity.incarnation = Incarnation::new([boot; 16]).unwrap();
        let checkpoint = self.manifest.as_ref().map(|bytes| {
            let manifest = CheckpointManifest::decode(bytes, config::limits()).unwrap();
            Checkpoint::from_connections(
                &manifest,
                self.attachments
                    .values()
                    .filter_map(|bytes| ConnectionCheckpoint::decode(bytes, config::limits()).ok())
                    .collect(),
                config::limits(),
            )
            .unwrap()
        });
        let surviving: Vec<_> = self.live.iter().copied().collect();
        let authority = self.stored.clone();
        let recovery = if self.initialized {
            Recovery::Existing {
                authority: authority.as_ref(),
                checkpoint: checkpoint.as_ref(),
                surviving: &surviving,
            }
        } else {
            Recovery::New
        };
        let runtime = Runtime::restore(self, self.identity, recovery, ())?;
        self.initialized = true;
        Ok(runtime)
    }
    fn open(&mut self, runtime: &mut Runtime<()>, id: u64) {
        self.live.insert(connection(id));
        runtime
            .event(self, Input::Connected(connection(id)))
            .unwrap();
    }
    fn frame(&mut self, runtime: &mut Runtime<()>, f: &Fixture, id: u64, key: &str) {
        control(
            runtime,
            self,
            connection(id),
            &f.frame(key),
            windshare_relay_core::access::RegistrationDecision::Granted(
                windshare_relay_core::access::Attribution::Anonymous,
            ),
        )
        .unwrap();
    }
    fn registered(&mut self, runtime: &mut Runtime<()>, f: &Fixture) {
        let writes = self.writes;
        self.open(runtime, 1);
        for key in ["registerInitB64", "registerProofB64", "descriptorUploadB64"] {
            self.frame(runtime, f, 1, key);
        }
        assert_eq!(self.writes, writes + 1);
        assert!(matches!(
            self.stored.as_ref().unwrap().record(),
            Record::Published(_)
        ));
        assert!(
            self.sent
                .iter()
                .any(|(_, bytes)| bytes == &f.frame("registeredB64"))
        );
    }
}
fn control(
    runtime: &mut Runtime<()>,
    host: &mut Memory,
    connection: ConnectionRef,
    bytes: &[u8],
    decision: windshare_relay_core::access::RegistrationDecision,
) -> Result<()> {
    use windshare_relay_core::ingress::Control;
    let control = if bytes.len() > config::MAX_CONTROL_BYTES {
        Control::Ready(Input::Unsupported(connection))
    } else {
        Control::inspect(connection, bytes)
    };
    let input = match control {
        Control::Authorize(request) => Input::Register(request.resolve(decision, host.now())),
        Control::Ready(input) => input,
    };
    runtime.event(host, input)
}

impl Host for Memory {
    fn now(&self) -> UnixMillis {
        self.time
    }
    fn challenge(&mut self) -> Result<ChallengeSeed> {
        Ok(self.seed.take().expect("one challenge"))
    }
    fn execute(&mut self, operation: &Storage) -> Result<Option<StoredAuthority>> {
        self.writes += 1;
        let failure = self.fail_write.filter(|(index, _)| *index == self.writes);
        if failure == Some((self.writes, false)) {
            bail!("injected before commit");
        }
        self.stored = authority::apply(
            self.identity.relay,
            self.identity.share,
            self.stored.as_ref(),
            operation,
        )
        .map_err(|error| anyhow::anyhow!("{error:?}"))?;
        if let Some(stored) = &self.stored {
            let row = crate::persistence::AuthorityRow::new(stored);
            let row: crate::persistence::AuthorityRow =
                serde_json::from_slice(&serde_json::to_vec(&row)?)?;
            self.stored = Some(row.decode(self.identity.relay, self.identity.share)?);
        }
        if failure.is_some() {
            bail!("injected lost receipt");
        }
        Ok(self.stored.clone())
    }
    fn send(&mut self, connection: ConnectionRef, bytes: &[u8]) -> Result<()> {
        if self.fail_send == Some(connection) {
            self.fail_send = None;
            bail!("uncertain handoff");
        }
        if !self.live.contains(&connection) {
            bail!("disconnected");
        }
        self.sent.push((connection, bytes.to_vec()));
        Ok(())
    }
    fn close(&mut self, connection: ConnectionRef) -> Result<()> {
        self.live.remove(&connection);
        self.attachments.remove(&connection);
        Ok(())
    }
    fn checkpoint(&mut self, target: CheckpointTarget, bytes: &[u8]) -> Result<()> {
        match target {
            CheckpointTarget::Manifest => {
                self.manifest = Some(bytes.to_vec());
                self.manifest_writes += 1;
            }
            CheckpointTarget::Generation(connection) => {
                if self.failed_attachments > 0 {
                    self.failed_attachments -= 1;
                    bail!("attachment write failed");
                }
                if self.live.contains(&connection) {
                    self.attachments.insert(connection, bytes.to_vec());
                }
            }
        }
        Ok(())
    }
}

#[test]
fn hibernation_retains_nonce_and_absolute_deadline_without_sql_heartbeat_writes() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    host.registered(&mut runtime, &f);
    host.frame(&mut runtime, &f, 1, "connectionProbeB64");
    let deadline = runtime.deadline();
    host.time.0 += 10_000;
    drop(runtime);
    let mut runtime = host.restore(2).unwrap();
    assert_eq!(runtime.deadline(), deadline);
    let writes = host.manifest_writes;
    let nonce = f.0["connectionProbeNonce"]
        .as_str()
        .unwrap()
        .parse::<u64>()
        .unwrap()
        + 1;
    let frame =
        windshare_relay_core::heartbeat::ConnectionProbe::new(NonZeroU64::new(nonce).unwrap())
            .encode();
    control(
        &mut runtime,
        &mut host,
        connection(1),
        &frame,
        windshare_relay_core::access::RegistrationDecision::Denied,
    )
    .unwrap();
    assert_eq!(host.manifest_writes, writes);
    assert_eq!(&host.sent.last().unwrap().1[..4], b"WS2A");
    drop(runtime);
    let mut runtime = host.restore(3).unwrap();
    control(
        &mut runtime,
        &mut host,
        connection(1),
        &frame,
        windshare_relay_core::access::RegistrationDecision::Denied,
    )
    .unwrap();
    assert!(
        !host.live.contains(&connection(1)),
        "hibernation must not forget the nonce"
    );
}

#[test]
fn pending_handshake_and_partial_attachment_write_recover_without_new_deadline() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    host.failed_attachments = 1;
    host.open(&mut runtime, 1);
    host.frame(&mut runtime, &f, 1, "registerInitB64");
    let deadline = runtime.deadline();
    drop(runtime);
    let mut runtime = host.restore(2).unwrap();
    assert_eq!(runtime.deadline(), deadline);
    host.frame(&mut runtime, &f, 1, "registerProofB64");
    host.frame(&mut runtime, &f, 1, "descriptorUploadB64");
    assert!(matches!(
        host.stored.as_ref().unwrap().record(),
        Record::Published(_)
    ));
}

#[test]
fn expiry_is_processed_before_a_late_heartbeat_and_grace_survives_reconstruction() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    host.registered(&mut runtime, &f);
    host.time = runtime.deadline().unwrap();
    drop(runtime);
    let mut runtime = host.restore(2).unwrap();
    assert!(host.live.is_empty());
    assert!(matches!(
        host.stored.as_ref().unwrap().record(),
        Record::Grace(_)
    ));
    let grace = runtime.deadline();
    control(
        &mut runtime,
        &mut host,
        connection(1),
        &f.frame("connectionProbeB64"),
        windshare_relay_core::access::RegistrationDecision::Denied,
    )
    .unwrap();
    assert_eq!(runtime.deadline(), grace);
    drop(runtime);
    let runtime = host.restore(3).unwrap();
    assert_eq!(runtime.deadline(), grace);
}

#[test]
fn uncertain_publication_never_acknowledges_and_recovers_from_authoritative_sql() {
    let f = Fixture::new();
    for committed in [false, true] {
        let mut host = Memory::new(&f);
        let mut runtime = host.restore(1).unwrap();
        host.open(&mut runtime, 1);
        host.frame(&mut runtime, &f, 1, "registerInitB64");
        host.frame(&mut runtime, &f, 1, "registerProofB64");
        host.fail_write = Some((1, committed));
        assert!(
            control(
                &mut runtime,
                &mut host,
                connection(1),
                &f.frame("descriptorUploadB64"),
                windshare_relay_core::access::RegistrationDecision::Granted(
                    windshare_relay_core::access::Attribution::Anonymous
                )
            )
            .is_err()
        );
        assert!(
            !host
                .sent
                .iter()
                .any(|(_, bytes)| bytes == &f.frame("registeredB64"))
        );
        drop(runtime);
        host.live.clear();
        host.attachments.clear();
        host.fail_write = None;
        let _runtime = host.restore(2).unwrap();
        assert_eq!(host.stored.is_some(), committed);
        if let Some(row) = &host.stored {
            assert!(matches!(row.record(), Record::Grace(_)));
            assert!(row.descriptor().is_some());
        }
    }
}

#[test]
fn stop_persists_before_ack_and_survives_empty_socket_reconstruction() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    host.time = f.time("stop");
    host.seed = Some(f.seed("stop"));
    let mut runtime = host.restore(1).unwrap();
    host.open(&mut runtime, 2);
    host.frame(&mut runtime, &f, 2, "stopInitB64");
    host.time = f.time("");
    host.seed = Some(f.seed(""));
    host.registered(&mut runtime, &f);
    control(
        &mut runtime,
        &mut host,
        connection(2),
        &f.frame("stopProofB64"),
        windshare_relay_core::access::RegistrationDecision::Denied,
    )
    .unwrap();
    assert!(matches!(
        host.stored.as_ref().unwrap().record(),
        Record::Stopped(_)
    ));
    assert!(host.stored.as_ref().unwrap().descriptor().is_none());
    assert!(
        host.sent
            .iter()
            .any(|(_, bytes)| bytes == &f.frame("stoppedB64"))
    );
    drop(runtime);
    let mut runtime = host.restore(2).unwrap();
    host.open(&mut runtime, 3);
    host.frame(&mut runtime, &f, 3, "registerInitB64");
    assert!(
        host.sent
            .iter()
            .any(|(c, bytes)| *c == connection(3) && bytes == &f.frame("stoppedErrorB64"))
    );
}

#[test]
fn deployment_permission_and_delivery_boundary_precede_admission() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    host.open(&mut runtime, 1);
    control(
        &mut runtime,
        &mut host,
        connection(1),
        &f.frame("registerInitB64"),
        windshare_relay_core::access::RegistrationDecision::Denied,
    )
    .unwrap();
    assert!(host.stored.is_none());
    assert!(!host.live.contains(&connection(1)));
    host.open(&mut runtime, 2);
    host.frame(&mut runtime, &f, 2, "joinB64");
    assert!(!host.live.contains(&connection(2)));
    assert!(host.stored.is_none());
}

#[test]
fn business_saturation_keeps_signed_stop_available_through_hibernation() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    host.time = f.time("stop");
    let mut runtime = host.restore(1).unwrap();
    let business_limit = config::limits().business_connections as u64;
    for id in 1..=business_limit {
        host.open(&mut runtime, id);
        // A RESUME reserves business capacity while awaiting its credential.
        host.frame(&mut runtime, &f, id, "resumeInitB64");
    }
    let refused = business_limit + 1;
    host.open(&mut runtime, refused);
    host.frame(&mut runtime, &f, refused, "resumeInitB64");
    assert!(!host.live.contains(&connection(refused)));
    let stop = business_limit + 2;
    host.time = f.time("stop");
    host.seed = Some(f.seed("stop"));
    host.open(&mut runtime, stop);
    control(
        &mut runtime,
        &mut host,
        connection(stop),
        &f.frame("stopInitB64"),
        windshare_relay_core::access::RegistrationDecision::Denied,
    )
    .unwrap();
    assert!(host.live.contains(&connection(stop)));
    let deadline = runtime.deadline();
    drop(runtime);
    let mut runtime = host.restore(2).unwrap();
    assert_eq!(runtime.deadline(), deadline);
    control(
        &mut runtime,
        &mut host,
        connection(stop),
        &f.frame("stopProofB64"),
        windshare_relay_core::access::RegistrationDecision::Denied,
    )
    .unwrap();
    assert!(matches!(
        host.stored.as_ref().unwrap().record(),
        Record::Stopped(_)
    ));
    assert!(
        host.sent
            .iter()
            .any(|(target, bytes)| *target == connection(stop) && bytes == &f.frame("stoppedB64"))
    );
    assert!(!host.live.contains(&connection(stop)));
}

#[test]
fn empty_socket_reconstruction_preserves_attempt_rate_history() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    let burst = u64::from(config::limits().handshakes.attempts.burst);
    for id in 1..=burst {
        host.open(&mut runtime, id);
        assert!(host.live.contains(&connection(id)));
        control(
            &mut runtime,
            &mut host,
            connection(id),
            b"invalid",
            windshare_relay_core::access::RegistrationDecision::Denied,
        )
        .unwrap();
    }
    assert!(host.live.is_empty());
    drop(runtime);
    let mut runtime = host.restore(2).unwrap();
    host.open(&mut runtime, burst + 1);
    assert!(
        host.live.is_empty(),
        "reconstruction must not refund admission attempts"
    );
    host.time.0 += u64::try_from(config::limits().handshakes.attempts.refill.as_millis()).unwrap();
    host.open(&mut runtime, burst + 2);
    assert!(host.live.contains(&connection(burst + 2)));
}
