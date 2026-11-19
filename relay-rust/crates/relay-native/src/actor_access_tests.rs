use super::{Actor, Event, Frozen, Generation, Recovery, Runtime, read};
use crate::{
    connection::{self, Writer},
    node::access::{AuthorityState, Bearer, Policy, TrustedIssuer},
    storage::Database,
    test_support::{Fixture, connection as reference, socket_admission},
};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ed25519_dalek::SigningKey;
use std::{
    collections::VecDeque,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64},
    },
    time::Duration,
};
use tokio::{sync::mpsc, time::timeout};
use windshare_relay_access::{
    Credential, CredentialState, Digest,
    entitlement::{DAY_SECONDS, ENTITLEMENT_LIFETIME, Entitlement},
    ticket::Ticket,
    trust::Generation as SigningGeneration,
    usage::NodePolicy,
};
use windshare_relay_core::error_frame::ErrorFrame;

const OWNER_KEY: &str = "test-owner-key";
const SERVICE_KEY: &str = "test-local-service-key";
const TEST_BUDGET: Duration = Duration::from_secs(3);

struct Sender {
    tx: mpsc::Sender<Event>,
    writer: Writer,
}

async fn with_sender(
    node: &Runtime,
    fixture: &Fixture,
    bearer: Option<&str>,
    exercise: impl AsyncFnOnce(&mut Sender),
) {
    let identity = fixture.identity();
    let directory = tempfile::tempdir().unwrap();
    let store = Database::open(&directory.path().join("authority.sqlite"), identity.relay).unwrap();
    let generation = Generation::new(identity, store.clone());
    let recovery = Recovery::vacant(&store);
    let environment = Frozen {
        now: Arc::new(AtomicU64::new(fixture.now(false).0)),
        seeds: Arc::new(Mutex::new(VecDeque::from([fixture.seed(false)]))),
    };
    let (tx, inbox) = mpsc::channel(4);
    let actor = tokio::spawn(generation.track(Actor::start(
        store,
        environment,
        inbox,
        Arc::new(AtomicBool::new(false)),
        generation.clone(),
        recovery,
        node.clone(),
    )));
    let admission = socket_admission();
    if let Some(bearer) = bearer {
        let mut bearer = Bearer::new(bearer);
        node.access.admission_priority(
            fixture.identity().share,
            Some(&mut bearer),
            fixture.now(false).0 / 1000,
        );
        admission.set_bearer(bearer);
    }
    let (port, writer, _) = connection::channel(reference(1), node.clone());
    tx.send(Event::Connected(
        reference(1),
        Box::new(port.with_admission(&admission)),
    ))
    .await
    .unwrap();
    let mut sender = Sender { tx, writer };
    sender.send(fixture.bytes("registerInitB64")).await;
    exercise(&mut sender).await;
    drop(sender);
    timeout(TEST_BUDGET, actor).await.unwrap().unwrap().unwrap();
    drop(directory);
}

impl Sender {
    async fn send(&self, frame: Vec<u8>) {
        self.tx
            .send(Event::Control(reference(1), frame))
            .await
            .unwrap();
    }

    async fn finish_registration(&mut self, fixture: &Fixture) {
        let challenge = timeout(TEST_BUDGET, self.writer.recv())
            .await
            .unwrap()
            .expect("relay closed before issuing the registration challenge");
        assert_eq!(&challenge[..4], b"WS2Q");
        self.send(fixture.bytes("registerProofB64")).await;
        self.send(fixture.bytes("descriptorUploadB64")).await;
        assert_eq!(read(&mut self.writer).await, fixture.bytes("registeredB64"));
    }
}

fn local_credential(secret: &str, id: u8) -> Credential {
    Credential {
        principal: [id; 16],
        id: [id; 16],
        name: secret.to_owned(),
        digest: Digest::of(secret.as_bytes()),
        created_at: 0,
        last_used_at: None,
        state: CredentialState::Active,
    }
}

#[tokio::test]
async fn public_pause_keeps_local_registration_and_revokes_public_access() {
    let fixture = Fixture::new();
    let now = fixture.now(false).0 / 1000;
    let root = SigningKey::from_bytes(&[1; 32]);
    let key = SigningKey::from_bytes(&[2; 32]);
    let generation = SigningGeneration {
        issuer: root.verifying_key().to_bytes(),
        number: 1,
        key: key.verifying_key().to_bytes(),
        not_before: now - 100,
        expires_at: now + DAY_SECONDS,
    };
    let ticket = Ticket {
        issuer: generation.issuer,
        key_id: generation.number,
        account: [3; 16],
        credential: [4; 16],
        issued_at: now,
        expires_at: now + 3600,
        entitlement: Entitlement {
            version: 1,
            day: now / DAY_SECONDS,
            valid_until: now + ENTITLEMENT_LIFETIME,
            remaining_bytes: 1000,
        },
    };
    let account_bearer = URL_SAFE_NO_PAD.encode(ticket.issue(generation, &key).unwrap());
    let owner = local_credential(OWNER_KEY, 5);
    let policy = Policy {
        owner_principals: vec![owner.principal],
        credentials: vec![owner, local_credential(SERVICE_KEY, 6)],
        trusted_issuers: vec![TrustedIssuer {
            root: URL_SAFE_NO_PAD.encode(generation.issuer),
            certificate: URL_SAFE_NO_PAD.encode(generation.certify(&root).unwrap()),
        }],
        ..Policy::default()
    };
    let node = || {
        let node = Runtime::default();
        node.access.configure(policy.clone()).unwrap();
        node.access.set_controller(Some(generation.issuer));
        node
    };
    let pause = |node: &Runtime| {
        node.access
            .apply_authority(AuthorityState {
                generation,
                policy: Some(NodePolicy {
                    version: 1,
                    enabled: false,
                    anonymous: true,
                }),
            })
            .unwrap();
    };

    let local = node();
    pause(&local);
    for key in [OWNER_KEY, SERVICE_KEY] {
        with_sender(&local, &fixture, Some(key), async |sender| {
            sender.finish_registration(&fixture).await;
        })
        .await;
    }

    for bearer in [None, Some(account_bearer.as_str())] {
        let public = node();
        with_sender(&public, &fixture, bearer, async |sender| {
            sender.finish_registration(&fixture).await;
            pause(&public);
            assert_eq!(
                timeout(TEST_BUDGET, sender.writer.recv()).await.unwrap(),
                None
            );
        })
        .await;

        with_sender(&public, &fixture, bearer, async |sender| {
            assert_eq!(
                read(&mut sender.writer).await,
                ErrorFrame::AccessDenied.encode()
            );
        })
        .await;
    }
}
