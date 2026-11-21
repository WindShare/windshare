use super::{Actor, Event, Frozen, Generation, Recovery, Runtime, read};
use crate::{
    connection::{self, Writer},
    node::access::{AuthorityState, Bearer, Policy, TrustedIssuer},
    storage::{Database, Persistence},
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
    restrictions::{Restriction, Restrictions},
    ticket::Ticket,
    trust::Generation as SigningGeneration,
    usage::{Admission, NodePolicy},
};
use windshare_relay_core::{challenge::ChallengeSeed, error_frame::ErrorFrame};

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
) -> bool {
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
        store.clone(),
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
    let persisted = store.load(identity.share).unwrap().is_some();
    drop(directory);
    persisted
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

fn access_fixture(now: u64) -> (SigningGeneration, String, Policy) {
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
    (generation, account_bearer, policy)
}

fn set_admission(
    node: &Runtime,
    generation: SigningGeneration,
    version: u64,
    admission: Admission,
) {
    node.access.set_controller(Some(generation.issuer));
    node.access
        .apply_authority(AuthorityState {
            generation,
            policy: Some(NodePolicy {
                version,
                admission,
                probe_account: windshare_relay_access::probe::PROBE_ACCOUNT,
                anonymous: true,
            }),
        })
        .unwrap();
}

#[tokio::test]
async fn disabling_closes_every_live_sender_and_denies_fresh_registrations() {
    let fixture = Fixture::new();
    let (generation, account, policy) = access_fixture(fixture.now(false).0 / 1000);
    for bearer in [
        None,
        Some(account.as_str()),
        Some(OWNER_KEY),
        Some(SERVICE_KEY),
    ] {
        let node = Runtime::default();
        node.access.configure(policy.clone()).unwrap();
        set_admission(&node, generation, 1, Admission::Accepting);
        with_sender(&node, &fixture, bearer, async |sender| {
            sender.finish_registration(&fixture).await;
            set_admission(&node, generation, 2, Admission::Disabled);
            assert_closed(&mut sender.writer).await;
        })
        .await;
        with_sender(&node, &fixture, bearer, async |sender| {
            assert_refused(&mut sender.writer, ErrorFrame::AccessDenied).await;
        })
        .await;
    }
}

#[tokio::test]
async fn public_daily_budget_exhaustion_refuses_new_public_registrations_and_disconnects_active_public_shares()
 {
    use crate::node::{ContributionLimits, Settings};
    use windshare_relay_core::error_frame::RetryAfter;

    const BUDGET_RETRY_MS: u32 = 30_000;
    let fixture = Fixture::new();
    let (_generation, account_bearer, policy) = access_fixture(fixture.now(false).0 / 1000);
    let base_settings = Settings {
        access: policy,
        contribution: ContributionLimits {
            daily_bytes: Some(1024),
            ..Default::default()
        },
        ..Settings::default()
    };
    let exhausted_settings = Settings {
        contribution: ContributionLimits {
            daily_bytes: Some(0),
            ..Default::default()
        },
        ..base_settings.clone()
    };
    let wall_now = i64::try_from(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs(),
    )
    .unwrap();
    let open_node = |settings: &Settings| {
        let dir = tempfile::tempdir().unwrap();
        let store =
            Database::open(&dir.path().join("node.sqlite"), fixture.identity().relay).unwrap();
        let node = Runtime::default();
        node.configure(settings.clone(), store).unwrap();
        (dir, node)
    };

    for bearer in [None, Some(account_bearer.as_str())] {
        let (_dir, public_node) = open_node(&base_settings);
        assert!(
            with_sender(&public_node, &fixture, bearer, async |sender| {
                sender.finish_registration(&fixture).await;
                public_node
                    .update(exhausted_settings.clone(), wall_now)
                    .unwrap();
                assert_eq!(
                    timeout(TEST_BUDGET, sender.writer.recv()).await.unwrap(),
                    None
                );
            })
            .await
        );

        let persisted = with_sender(&public_node, &fixture, bearer, async |sender| {
            assert_eq!(
                read(&mut sender.writer).await,
                ErrorFrame::AccessUnavailable(RetryAfter::from_millis(BUDGET_RETRY_MS).unwrap())
                    .encode()
            );
        })
        .await;
        assert!(!persisted);
    }

    let (_dir, owner_node) = open_node(&base_settings);
    assert!(
        with_sender(&owner_node, &fixture, Some(OWNER_KEY), async |sender| {
            sender.finish_registration(&fixture).await;
            owner_node
                .update(exhausted_settings.clone(), wall_now)
                .unwrap();
            assert!(
                timeout(Duration::from_millis(50), sender.writer.recv())
                    .await
                    .is_err()
            );
        })
        .await
    );
    assert!(
        with_sender(&owner_node, &fixture, Some(OWNER_KEY), async |sender| {
            sender.finish_registration(&fixture).await;
        })
        .await
    );
}

const RESUME_HANDSHAKE_BUDGET_MS: u64 = 5_000;
const ACCOUNT_ID: [u8; 16] = [3; 16];
const CREDENTIAL_ID: [u8; 16] = [4; 16];

fn resume_now(fixture: &Fixture) -> u64 {
    fixture.0["resumeExpiresAt"]
        .as_str()
        .unwrap()
        .parse::<u64>()
        .unwrap()
        * 1000
        - RESUME_HANDSHAKE_BUDGET_MS
        - 1
}

fn resume_seed(fixture: &Fixture) -> ChallengeSeed {
    ChallengeSeed::new(
        fixture.bytes("resumeChallengeIdB64").try_into().unwrap(),
        fixture.bytes("resumeChallengeNonceB64").try_into().unwrap(),
    )
    .unwrap()
}

struct AccessHarness {
    tx: mpsc::Sender<Event>,
    node: Runtime,
    now: Arc<AtomicU64>,
}

async fn with_access_actor(
    node: Runtime,
    store: Database,
    fixture: &Fixture,
    exercise: impl AsyncFnOnce(&AccessHarness),
) {
    let identity = fixture.identity();
    let generation = Generation::new(identity, store.clone());
    let now = Arc::new(AtomicU64::new(fixture.now(false).0));
    let environment = Frozen {
        now: Arc::clone(&now),
        seeds: Arc::new(Mutex::new(VecDeque::from([
            fixture.seed(false),
            resume_seed(fixture),
        ]))),
    };
    let (tx, inbox) = mpsc::channel(16);
    let actor = tokio::spawn(generation.track(Actor::start(
        store.clone(),
        environment,
        inbox,
        Arc::new(AtomicBool::new(false)),
        generation.clone(),
        Recovery::vacant(&store),
        node.clone(),
    )));
    let harness = AccessHarness { tx, node, now };
    exercise(&harness).await;
    drop(harness);
    timeout(TEST_BUDGET, actor).await.unwrap().unwrap().unwrap();
}

impl AccessHarness {
    async fn connect(&self, id: u64, bearer: Option<&str>, fixture: &Fixture) -> Writer {
        let admission = socket_admission();
        if let Some(bearer) = bearer {
            let mut bearer = Bearer::new(bearer);
            self.node.access.admission_priority(
                fixture.identity().share,
                Some(&mut bearer),
                fixture.now(false).0 / 1000,
            );
            admission.set_bearer(bearer);
        }
        let (port, writer, _) = connection::channel(reference(id), self.node.clone());
        self.tx
            .send(Event::Connected(
                reference(id),
                Box::new(port.with_admission(&admission)),
            ))
            .await
            .unwrap();
        writer
    }

    async fn send(&self, id: u64, frame: Vec<u8>) {
        self.tx
            .send(Event::Control(reference(id), frame))
            .await
            .unwrap();
    }

    async fn publish(&self, id: u64, bearer: Option<&str>, fixture: &Fixture) -> Writer {
        let mut writer = self.connect(id, bearer, fixture).await;
        self.send(id, fixture.bytes("registerInitB64")).await;
        let challenge = read(&mut writer).await;
        assert_eq!(&challenge[..4], b"WS2Q");
        self.send(id, fixture.bytes("registerProofB64")).await;
        self.send(id, fixture.bytes("descriptorUploadB64")).await;
        assert_eq!(read(&mut writer).await, fixture.bytes("registeredB64"));
        writer
    }

    async fn start_resume(&self, id: u64, bearer: Option<&str>, fixture: &Fixture) -> Writer {
        self.now
            .store(resume_now(fixture), std::sync::atomic::Ordering::Release);
        let writer = self.connect(id, bearer, fixture).await;
        self.send(id, fixture.bytes("resumeInitB64")).await;
        self.send(id, fixture.bytes("resumeCredentialB64")).await;
        writer
    }

    async fn send_join(&self, id: u64, fixture: &Fixture) -> Writer {
        let writer = self.connect(id, None, fixture).await;
        self.send(id, fixture.bytes("joinB64")).await;
        writer
    }
}

async fn assert_closed(writer: &mut Writer) {
    assert_eq!(timeout(TEST_BUDGET, writer.recv()).await.unwrap(), None);
}

async fn assert_refused(writer: &mut Writer, expected: ErrorFrame) {
    assert_eq!(read(writer).await, expected.encode());
    assert_closed(writer).await;
}

#[derive(Clone, Copy)]
enum PolicyRevocation {
    ShareBan,
    AccountBan,
    CredentialRevocation,
    LocalServiceKeyRevocation,
    Disabled,
}

fn apply_policy_revocation(
    kind: PolicyRevocation,
    node: &Runtime,
    generation: SigningGeneration,
    policy: &Policy,
    share: [u8; 12],
    now: u64,
) {
    match kind {
        PolicyRevocation::ShareBan => {
            let mut restrictions = Restrictions {
                version: 1,
                ..Restrictions::default()
            };
            restrictions.insert(Restriction::Share(share));
            assert!(node.access.restrict(generation.issuer, restrictions));
        }
        PolicyRevocation::AccountBan => {
            let mut restrictions = Restrictions {
                version: 1,
                ..Restrictions::default()
            };
            restrictions.insert(Restriction::Account(ACCOUNT_ID));
            assert!(node.access.restrict(generation.issuer, restrictions));
        }
        PolicyRevocation::CredentialRevocation => {
            let mut restrictions = Restrictions {
                version: 1,
                ..Restrictions::default()
            };
            restrictions.insert(Restriction::Credential(CREDENTIAL_ID));
            assert!(node.access.restrict(generation.issuer, restrictions));
        }
        PolicyRevocation::LocalServiceKeyRevocation => {
            let mut updated = policy.clone();
            for credential in &mut updated.credentials {
                if credential.name == SERVICE_KEY {
                    credential.state = CredentialState::Revoked { at: now };
                }
            }
            node.access.configure(updated).unwrap();
        }
        PolicyRevocation::Disabled => {
            set_admission(node, generation, 2, Admission::Disabled);
        }
    }
}

#[tokio::test]
async fn revoked_resident_share_refuses_resume_and_join_after_access_change_is_consumed() {
    use windshare_relay_core::lifecycle::Record;

    let fixture = Fixture::new();
    let now_seconds = fixture.now(false).0 / 1000;
    let (generation, account_bearer, policy) = access_fixture(now_seconds);
    let share_bytes = *fixture.identity().share.as_bytes();
    let cases = [
        (
            "share_ban",
            Some(account_bearer.as_str()),
            PolicyRevocation::ShareBan,
        ),
        (
            "account_ban",
            Some(account_bearer.as_str()),
            PolicyRevocation::AccountBan,
        ),
        (
            "credential_revocation",
            Some(account_bearer.as_str()),
            PolicyRevocation::CredentialRevocation,
        ),
        (
            "local_service_key_revocation",
            Some(SERVICE_KEY),
            PolicyRevocation::LocalServiceKeyRevocation,
        ),
        (
            "public_pause_account",
            Some(account_bearer.as_str()),
            PolicyRevocation::Disabled,
        ),
        ("public_pause_anonymous", None, PolicyRevocation::Disabled),
    ];

    for (case, bearer, revocation) in cases {
        let directory = tempfile::tempdir().unwrap();
        let store = Database::open(
            &directory.path().join("authority.sqlite"),
            fixture.identity().relay,
        )
        .unwrap();
        let node = Runtime::default();
        node.access.configure(policy.clone()).unwrap();
        set_admission(&node, generation, 1, Admission::Accepting);

        with_access_actor(node.clone(), store.clone(), &fixture, async |harness| {
            let mut sender = harness.publish(1, bearer, &fixture).await;
            apply_policy_revocation(
                revocation,
                &node,
                generation,
                &policy,
                share_bytes,
                now_seconds,
            );
            assert_closed(&mut sender).await;

            let mut resumed = harness.start_resume(2, bearer, &fixture).await;
            assert_refused(&mut resumed, ErrorFrame::AccessDenied).await;

            let mut joined = harness.send_join(3, &fixture).await;
            assert_refused(&mut joined, ErrorFrame::AccessDenied).await;
        })
        .await;

        assert!(
            matches!(
                store
                    .load(fixture.identity().share)
                    .unwrap()
                    .map(|stored| stored.record()),
                Some(Record::Grace(_))
            ),
            "{case}: revoked share must remain in Grace rather than resurrecting"
        );
    }
}

#[tokio::test]
async fn draining_preserves_join_and_resume_for_an_existing_owner_share() {
    let fixture = Fixture::new();
    let now_seconds = fixture.now(false).0 / 1000;
    let (generation, _account_bearer, policy) = access_fixture(now_seconds);
    let directory = tempfile::tempdir().unwrap();
    let store = Database::open(
        &directory.path().join("authority.sqlite"),
        fixture.identity().relay,
    )
    .unwrap();
    let node = Runtime::default();
    assert!(
        node.maintenance
            .set(crate::node::maintenance::Gate::install(store.path()).unwrap())
            .is_ok()
    );
    let fence = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open(store.path().with_added_extension("admission"))
        .unwrap();
    node.access.configure(policy).unwrap();
    set_admission(&node, generation, 1, Admission::Accepting);
    with_access_actor(node.clone(), store, &fixture, async |harness| {
        let _sender = harness.publish(1, Some(OWNER_KEY), &fixture).await;
        set_admission(&node, generation, 2, Admission::Draining);
        harness
            .tx
            .send(Event::Disconnected(reference(1)))
            .await
            .unwrap();
        let mut resumed = harness.start_resume(2, Some(OWNER_KEY), &fixture).await;
        assert_eq!(&read(&mut resumed).await[..4], b"WS2Q");
        harness.send(2, fixture.bytes("resumeProofB64")).await;
        assert_eq!(read(&mut resumed).await, fixture.bytes("registeredB64"));
        let mut joined = harness.send_join(3, &fixture).await;
        assert_eq!(&read(&mut joined).await[..4], b"WS2D");
        // A pending JOIN already owns the fence before its sender admits the session.
        assert!(matches!(
            fence.try_lock(),
            Err(std::fs::TryLockError::WouldBlock)
        ));
        let (closed, applied) = tokio::sync::oneshot::channel();
        harness
            .tx
            .send(Event::Disconnected(reference(3)))
            .await
            .unwrap();
        harness
            .tx
            .send(Event::Closed(reference(3), closed))
            .await
            .unwrap();
        applied.await.unwrap();
        fence.try_lock().unwrap();
        let mut retrying = harness.send_join(4, &fixture).await;
        assert_refused(
            &mut retrying,
            ErrorFrame::AccessUnavailable(
                windshare_relay_core::error_frame::RetryAfter::from_millis(1000).unwrap(),
            ),
        )
        .await;
    })
    .await;
}

#[tokio::test]
async fn restarted_actor_refuses_resume_and_join_for_previously_banned_share() {
    let fixture = Fixture::new();
    let now_seconds = fixture.now(false).0 / 1000;
    let (generation, account_bearer, policy) = access_fixture(now_seconds);
    let directory = tempfile::tempdir().unwrap();
    let store = Database::open(
        &directory.path().join("authority.sqlite"),
        fixture.identity().relay,
    )
    .unwrap();
    let node = Runtime::default();
    node.access.configure(policy.clone()).unwrap();
    set_admission(&node, generation, 1, Admission::Accepting);

    with_access_actor(node.clone(), store.clone(), &fixture, async |first| {
        let _sender = first
            .publish(1, Some(account_bearer.as_str()), &fixture)
            .await;
    })
    .await;

    apply_policy_revocation(
        PolicyRevocation::ShareBan,
        &node,
        generation,
        &policy,
        *fixture.identity().share.as_bytes(),
        now_seconds,
    );

    // Fresh actor consumes `access_changes` on startup with no ports attached.
    with_access_actor(node, store, &fixture, async |restarted| {
        let mut resumed = restarted
            .start_resume(2, Some(account_bearer.as_str()), &fixture)
            .await;
        assert_refused(&mut resumed, ErrorFrame::AccessDenied).await;
        let mut joined = restarted.send_join(3, &fixture).await;
        assert_refused(&mut joined, ErrorFrame::AccessDenied).await;
    })
    .await;
}

#[tokio::test]
async fn public_daily_budget_exhaustion_refuses_resume_and_join_with_retry_after() {
    use crate::node::{ContributionLimits, Settings};
    use windshare_relay_core::error_frame::RetryAfter;

    const BUDGET_RETRY_MS: u32 = 30_000;
    let fixture = Fixture::new();
    let (_generation, account_bearer, policy) = access_fixture(fixture.now(false).0 / 1000);
    let base_settings = Settings {
        access: policy,
        contribution: ContributionLimits {
            daily_bytes: Some(1024),
            ..Default::default()
        },
        ..Settings::default()
    };
    let exhausted_settings = Settings {
        contribution: ContributionLimits {
            daily_bytes: Some(0),
            ..Default::default()
        },
        ..base_settings.clone()
    };
    let wall_now = i64::try_from(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs(),
    )
    .unwrap();
    let expected = ErrorFrame::AccessUnavailable(RetryAfter::from_millis(BUDGET_RETRY_MS).unwrap());

    for bearer in [None, Some(account_bearer.as_str())] {
        let directory = tempfile::tempdir().unwrap();
        let node_store = Database::open(
            &directory.path().join("node.sqlite"),
            fixture.identity().relay,
        )
        .unwrap();
        let share_store = Database::open(
            &directory.path().join("authority.sqlite"),
            fixture.identity().relay,
        )
        .unwrap();
        let node = Runtime::default();
        node.configure(base_settings.clone(), node_store).unwrap();

        with_access_actor(node.clone(), share_store, &fixture, async |harness| {
            let mut sender = harness.publish(1, bearer, &fixture).await;
            node.update(exhausted_settings.clone(), wall_now).unwrap();
            assert_closed(&mut sender).await;

            let mut resumed = harness.start_resume(2, bearer, &fixture).await;
            assert_refused(&mut resumed, expected).await;
            let mut joined = harness.send_join(3, &fixture).await;
            assert_refused(&mut joined, expected).await;
        })
        .await;
    }
}

#[tokio::test]
async fn registration_authorized_traces_record_class_and_specific_refusal_reasons() {
    use crate::node::{ContributionLimits, Settings};
    use crate::test_support::capture_events_async;

    async fn capture_registration(
        node: &Runtime,
        fixture: &Fixture,
        bearer: Option<&str>,
    ) -> Vec<serde_json::Value> {
        capture_events_async(async {
            let _ = with_sender(node, fixture, bearer, async |sender| {
                let _ = read(&mut sender.writer).await;
            })
            .await;
        })
        .await
        .into_iter()
        .filter(|entry| entry["fields"]["event"] == "registration_authorized")
        .collect()
    }

    let fixture = Fixture::new();
    let (_generation, account_bearer, mut policy) = access_fixture(fixture.now(false).0 / 1000);
    policy.anonymous = false;
    let node = Runtime::default();
    node.access.configure(policy.clone()).unwrap();

    let granted = capture_registration(&node, &fixture, Some(&account_bearer)).await;
    assert_eq!(granted.len(), 1);
    assert_eq!(granted[0]["fields"]["class"], "Standard");
    assert_eq!(granted[0]["fields"]["allowance"], "Some(Available)");
    assert!(granted[0]["fields"].get("reason").is_none());

    let refused = capture_registration(&node, &fixture, None).await;
    assert_eq!(refused.len(), 1);
    assert_eq!(refused[0]["fields"]["reason"], "anonymous_disabled");
    assert!(refused[0]["fields"].get("class").is_none());

    let dir = tempfile::tempdir().unwrap();
    let store =
        Database::open(&dir.path().join("budget.sqlite"), fixture.identity().relay).unwrap();
    let exhausted = Runtime::default();
    exhausted
        .configure(
            Settings {
                access: policy,
                contribution: ContributionLimits {
                    daily_bytes: Some(0),
                    ..Default::default()
                },
                ..Settings::default()
            },
            store,
        )
        .unwrap();
    let budget_refused = capture_registration(&exhausted, &fixture, Some(&account_bearer)).await;
    assert_eq!(budget_refused.len(), 1);
    assert_eq!(
        budget_refused[0]["fields"]["reason"],
        "public_daily_budget_exhausted"
    );
}

#[tokio::test]
async fn maintenance_gate_io_errors_warn_and_do_not_report_node_draining() {
    use crate::test_support::capture_events_async;

    let fixture = Fixture::new();
    let now_seconds = fixture.now(false).0 / 1000;
    let (generation, _account_bearer, policy) = access_fixture(now_seconds);
    let directory = tempfile::tempdir().unwrap();
    let store = Database::open(
        &directory.path().join("authority.sqlite"),
        fixture.identity().relay,
    )
    .unwrap();
    let gate = crate::node::maintenance::Gate::install(store.path()).unwrap();
    let node = Runtime::default();
    assert!(node.maintenance.set(gate.clone()).is_ok());
    node.access.configure(policy).unwrap();
    set_admission(&node, generation, 1, Admission::Accepting);

    let auth_events = capture_events_async(async {
        gate.set_io_fault(Some(std::io::ErrorKind::PermissionDenied));
        let expected_auth =
            match windshare_relay_core::access::RegistrationDecision::SERVICE_UNAVAILABLE {
                windshare_relay_core::access::RegistrationDecision::Unavailable(retry) => {
                    ErrorFrame::AccessUnavailable(retry)
                }
                _ => unreachable!("SERVICE_UNAVAILABLE is Unavailable"),
            };
        let _ = with_sender(&node, &fixture, Some(OWNER_KEY), async |sender| {
            assert_refused(&mut sender.writer, expected_auth).await;
        })
        .await;
    })
    .await;
    assert!(auth_events.iter().any(|entry| {
        entry["level"] == "WARN"
            && entry["fields"]["event"] == "maintenance_gate_io_error"
            && entry["fields"]["operation"] == "authorize"
    }));
    let registration = auth_events
        .iter()
        .find(|entry| entry["fields"]["event"] == "registration_authorized")
        .expect("registration_authorized event");
    assert_eq!(registration["fields"]["reason"], "maintenance_gate_error");

    gate.set_io_fault(None);
    let join_events = capture_events_async(async {
        with_access_actor(node.clone(), store, &fixture, async |harness| {
            let _sender = harness.publish(1, Some(OWNER_KEY), &fixture).await;
            gate.set_io_fault(Some(std::io::ErrorKind::PermissionDenied));
            let mut joined = harness.send_join(2, &fixture).await;
            assert_refused(
                &mut joined,
                ErrorFrame::AccessUnavailable(
                    windshare_relay_core::error_frame::RetryAfter::from_millis(1000).unwrap(),
                ),
            )
            .await;
        })
        .await;
    })
    .await;
    assert!(join_events.iter().any(|entry| {
        entry["level"] == "WARN"
            && entry["fields"]["event"] == "maintenance_gate_io_error"
            && entry["fields"]["kind"] == "Join"
    }));
}

#[tokio::test]
async fn local_maintenance_fence_reports_distinct_refusal_reason_when_authority_accepts() {
    use crate::test_support::capture_events_async;

    let fixture = Fixture::new();
    let now_seconds = fixture.now(false).0 / 1000;
    let (generation, _account_bearer, policy) = access_fixture(now_seconds);
    let directory = tempfile::tempdir().unwrap();
    let store = Database::open(
        &directory.path().join("authority.sqlite"),
        fixture.identity().relay,
    )
    .unwrap();
    let gate = crate::node::maintenance::Gate::install(store.path()).unwrap();
    std::fs::write(
        store.path().with_added_extension("admission"),
        b"operation-stuck-1",
    )
    .unwrap();
    let node = Runtime::default();
    assert!(node.maintenance.set(gate).is_ok());
    node.access.configure(policy).unwrap();
    set_admission(&node, generation, 1, Admission::Accepting);

    let events = capture_events_async(async {
        let _ = with_sender(&node, &fixture, Some(OWNER_KEY), async |sender| {
            let _ = read(&mut sender.writer).await;
        })
        .await;
    })
    .await;
    let registration = events
        .iter()
        .find(|entry| entry["fields"]["event"] == "registration_authorized")
        .expect("registration_authorized event");
    assert_eq!(registration["fields"]["reason"], "local_maintenance");
}
