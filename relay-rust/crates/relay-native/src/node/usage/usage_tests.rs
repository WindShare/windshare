use super::*;
use crate::{
    Database, Endpoint,
    node::{Identity, Runtime},
};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use std::path::Path;
use windshare_relay_access::{
    Attribution,
    usage::{Batch, Load, UsageRecord},
};

fn usage_store(path: &Path) -> Database {
    let relay = Endpoint::parse("ws://localhost:8787/v2/ws")
        .unwrap()
        .identity;
    let store = Database::open(path, relay).unwrap();
    store.initialize_control(0).unwrap();
    store
}

#[test]
fn only_actual_writes_in_a_forwarding_scope_count_and_midnight_splits_days() {
    let collector = Collector::default();
    collector.configure([1; 32]);
    let meter = Meter::default();
    let account = Attribution::Account {
        issuer: [1; 32],
        account: [2; 16],
        credential: [3; 16],
    };
    meter.record(&collector, 1000, DAY_SECONDS - 1);
    let scope = meter.enter(account);
    meter.record(&collector, 7, DAY_SECONDS - 1);
    meter.record(&collector, 9, DAY_SECONDS);
    drop(scope);
    meter.record(&collector, 2000, DAY_SECONDS);
    let records = collector.drain();
    assert_eq!(records.len(), 2);
    assert_eq!(records[&([1; 32], [2; 16], 0)], 7);
    assert_eq!(records[&([1; 32], [2; 16], 1)], 9);
    collector.restore(records.clone());
    assert_eq!(collector.drain(), records);
}

#[tokio::test]
async fn flush_reads_the_clock_after_draining_midnight_usage() {
    let directory = tempfile::tempdir().unwrap();
    let store = usage_store(&directory.path().join("usage.db"));
    let issuer = [1; 32];
    let account = [2; 16];
    let reporter = Reporter::new(
        Runtime::default(),
        store.clone(),
        Identity::generate().unwrap().key(),
        Authority {
            url: "http://localhost".into(),
            issuer: URL_SAFE_NO_PAD.encode(issuer),
        },
    )
    .unwrap();
    let collector = &reporter.node.usage;
    let attribution = Attribution::Account {
        issuer,
        account,
        credential: [3; 16],
    };
    collector.record(attribution, 7, DAY_SECONDS - 1);
    collector.record(attribution, 9, DAY_SECONDS);

    reporter
        .flush(|| {
            collector.record(attribution, 11, DAY_SECONDS);
            i64::try_from(DAY_SECONDS).unwrap()
        })
        .await
        .unwrap();

    let node = reporter.signing_key.verifying_key().to_bytes();
    let batch = store.next_usage_batch(issuer, node).unwrap().unwrap();
    assert_eq!(batch.created_at, DAY_SECONDS);
    assert_eq!(
        batch.records,
        [
            UsageRecord {
                account,
                day: 0,
                bytes: 7
            },
            UsageRecord {
                account,
                day: 1,
                bytes: 9
            },
        ]
    );
    assert_eq!(
        collector.drain(),
        BTreeMap::from([((issuer, account, 1), 11)])
    );
}

fn usage_batch() -> Batch {
    Batch {
        id: [1; 16],
        node: [2; 32],
        issuer: [3; 32],
        created_at: DAY_SECONDS,
        records: vec![UsageRecord {
            account: [4; 16],
            day: 0,
            bytes: 17,
        }],
        resident_accounts: vec![],
        shares: vec![],
        load: Load {
            active_sessions: 0,
            wire_bytes: 17,
            forwarded_bytes: 17,
            admission_refusals: 0,
        },
    }
}

#[test]
fn persisted_batches_retain_ids_and_bytes_across_process_restart() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("usage.db");
    let store = usage_store(&path);
    let batch = usage_batch();
    store.enqueue_usage(std::slice::from_ref(&batch)).unwrap();
    drop(store);
    let store = usage_store(&path);
    assert_eq!(
        store.next_usage_batch(batch.issuer, batch.node).unwrap(),
        Some(batch.clone())
    );
    store.acknowledge_usage(batch.id).unwrap();
    assert!(
        store
            .next_usage_batch(batch.issuer, batch.node)
            .unwrap()
            .is_none()
    );
}

#[test]
fn unusable_outbox_entries_are_preserved_without_blocking_the_next_batch() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("usage.db");
    let store = usage_store(&path);
    let good = usage_batch();
    let mut invalid = good.clone();
    invalid.id = [2; 16];
    invalid.created_at = DAY_SECONDS - 1;
    invalid.records[0].day = 1;

    assert!(
        store
            .enqueue_usage(&[good.clone(), invalid.clone()])
            .is_err()
    );
    assert!(
        store
            .next_usage_batch(good.issuer, good.node)
            .unwrap()
            .is_none()
    );
    let mut old_identity = good.clone();
    old_identity.id = [4; 16];
    old_identity.node = [5; 32];
    old_identity.created_at = DAY_SECONDS - 1;
    store
        .enqueue_usage(&[old_identity.clone(), good.clone()])
        .unwrap();
    drop(store);

    // Seed a pre-fix batch and a damaged row outside the checked enqueue path.
    let entries = [
        (
            invalid.id,
            serde_json::to_vec(&invalid).unwrap(),
            "invalid_usage_batch",
        ),
        ([3; 16], b"{".to_vec(), "invalid_json"),
    ];
    let created_at = i64::try_from(invalid.created_at).unwrap();
    let sql = rusqlite::Connection::open(&path).unwrap();
    for (id, body, _) in &entries {
        sql.execute(
            "INSERT INTO usage_outbox VALUES(?1,?2,?3,?4)",
            rusqlite::params![id.as_slice(), good.issuer.as_slice(), body, created_at],
        )
        .unwrap();
    }
    drop(sql);

    let store = usage_store(&path);
    assert_eq!(
        store
            .next_usage_batch(good.issuer, good.node)
            .unwrap()
            .as_ref(),
        Some(&good)
    );
    drop(store);

    let store = usage_store(&path);
    assert_eq!(
        store
            .next_usage_batch(good.issuer, good.node)
            .unwrap()
            .as_ref(),
        Some(&good)
    );
    store.acknowledge_usage(good.id).unwrap();
    assert!(
        store
            .next_usage_batch(good.issuer, good.node)
            .unwrap()
            .is_none()
    );

    let sql = rusqlite::Connection::open(&path).unwrap();
    for (id, body, reason) in entries.into_iter().chain([(
        old_identity.id,
        serde_json::to_vec(&old_identity).unwrap(),
        "node_identity_mismatch",
    )]) {
        let saved: (Issuer, Vec<u8>, i64, String) = sql
            .query_row(
                "SELECT issuer,body,created_at,reason FROM usage_outbox_quarantine WHERE id=?1",
                [id.as_slice()],
                |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
            )
            .unwrap();
        assert_eq!(saved, (good.issuer, body, created_at, reason.to_owned()));
    }
}

struct FakeAuthority {
    root: ed25519_dalek::SigningKey,
    signing: ed25519_dalek::SigningKey,
    generation: windshare_relay_access::trust::Generation,
    certificate: String,
    daily_limit: u64,
    fail_usage: bool,
    restriction_version: u64,
    used_bytes: u64,
    entitlement_version: u64,
    order: Vec<&'static str>,
}

impl FakeAuthority {
    fn new(now: u64, daily_limit: u64, fail_usage: bool) -> Self {
        use windshare_relay_access::trust::Generation;
        let root = ed25519_dalek::SigningKey::from_bytes(&[11; 32]);
        let signing = ed25519_dalek::SigningKey::from_bytes(&[12; 32]);
        let generation = Generation {
            issuer: root.verifying_key().to_bytes(),
            number: 1,
            key: signing.verifying_key().to_bytes(),
            not_before: now.saturating_sub(60),
            expires_at: now.saturating_add(DAY_SECONDS),
        };
        let certificate = URL_SAFE_NO_PAD.encode(generation.certify(&root).unwrap());
        Self {
            root,
            signing,
            generation,
            certificate,
            daily_limit,
            fail_usage,
            restriction_version: 3,
            used_bytes: 0,
            entitlement_version: 0,
            order: Vec::new(),
        }
    }

    fn handle(&mut self, body: &[u8], now: u64) -> Option<Vec<u8>> {
        use windshare_relay_access::{
            entitlement::{ENTITLEMENT_LIFETIME, Entitlement},
            trust,
            usage::{
                AccountEntitlement, NodePolicy, RESPONSE_DOMAIN, Reply, ReportResult, SignedReply,
            },
        };
        let batch: Batch = serde_json::from_slice(&body[..body.len() - 64]).unwrap();
        let result = if batch.records.is_empty() {
            self.order.push("status");
            let day = now / DAY_SECONDS;
            let remaining = self.daily_limit.saturating_sub(self.used_bytes);
            let entitlements = batch
                .resident_accounts
                .iter()
                .map(|&account| {
                    self.entitlement_version += 1;
                    AccountEntitlement {
                        account,
                        entitlement: Entitlement {
                            version: self.entitlement_version,
                            day,
                            valid_until: now
                                .saturating_add(ENTITLEMENT_LIFETIME)
                                .min((day + 1) * DAY_SECONDS),
                            remaining_bytes: remaining,
                        },
                    }
                })
                .collect();
            ReportResult::Status {
                entitlements,
                restriction_version: self.restriction_version,
                policy: NodePolicy {
                    version: 1,
                    enabled: true,
                    anonymous: true,
                },
            }
        } else {
            self.order.push("usage");
            if self.fail_usage {
                return None;
            }
            for record in &batch.records {
                self.used_bytes = self.used_bytes.saturating_add(record.bytes);
            }
            ReportResult::UsageReceived
        };
        let reply = Reply {
            request: batch.id,
            node: batch.node,
            issuer: self.generation.issuer,
            generation: self.generation.number,
            issued_at: now,
            result,
        };
        let signed = SignedReply {
            certificate: self.certificate.clone(),
            body: URL_SAFE_NO_PAD.encode(trust::sign(
                RESPONSE_DOMAIN,
                &serde_json::to_vec(&reply).unwrap(),
                &self.signing,
            )),
        };
        Some(serde_json::to_vec(&signed).unwrap())
    }
}

async fn spawn_fake_authority(
    state: Arc<Mutex<FakeAuthority>>,
) -> (String, tokio::task::JoinHandle<()>) {
    use http_body_util::{BodyExt, Full};
    use hyper::{Response, StatusCode, body::Bytes, server::conn::http1, service::service_fn};
    use hyper_util::rt::TokioIo;

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let task = tokio::spawn(async move {
        while let Ok((stream, _)) = listener.accept().await {
            let state = Arc::clone(&state);
            tokio::spawn(async move {
                let service = service_fn(move |request: hyper::Request<hyper::body::Incoming>| {
                    let state = Arc::clone(&state);
                    async move {
                        let bytes = request.into_body().collect().await.unwrap().to_bytes();
                        let now = crate::node::now().unsigned_abs();
                        let payload = state.lock().unwrap().handle(&bytes, now);
                        let response = payload.map_or_else(
                            || {
                                Response::builder()
                                    .status(StatusCode::INTERNAL_SERVER_ERROR)
                                    .body(Full::new(Bytes::new()))
                                    .unwrap()
                            },
                            |json| {
                                Response::builder()
                                    .status(StatusCode::OK)
                                    .header("content-type", "application/json")
                                    .body(Full::new(Bytes::from(json)))
                                    .unwrap()
                            },
                        );
                        Ok::<_, hyper::Error>(response)
                    }
                });
                let _ = http1::Builder::new()
                    .serve_connection(TokioIo::new(stream), service)
                    .await;
            });
        }
    });
    (url, task)
}

fn cycle_reporter(
    directory: &Path,
    authority: &FakeAuthority,
    url: String,
) -> (Reporter, Database, Attribution) {
    use crate::node::{
        Settings,
        access::{Policy, TrustedIssuer},
    };
    let issuer = authority.root.verifying_key().to_bytes();
    let relay = Endpoint::parse("ws://localhost:8787/v2/ws")
        .unwrap()
        .identity;
    let store = Database::open(&directory.join("cycle.db"), relay).unwrap();
    let settings = Settings {
        access: Policy {
            trusted_issuers: vec![TrustedIssuer {
                root: URL_SAFE_NO_PAD.encode(issuer),
                certificate: authority.certificate.clone(),
            }],
            ..Policy::default()
        },
        ..Settings::default()
    };
    let node = Runtime::new(settings, store.clone()).unwrap();
    let reporter = Reporter::new(
        node,
        store.clone(),
        Identity::generate().unwrap().key(),
        Authority {
            url,
            issuer: URL_SAFE_NO_PAD.encode(issuer),
        },
    )
    .unwrap();
    let attribution = Attribution::Account {
        issuer,
        account: [9; 16],
        credential: [10; 16],
    };
    (reporter, store, attribution)
}

#[tokio::test]
async fn report_cycle_submits_usage_before_refreshing_entitlements() {
    use windshare_relay_access::entitlement::{Allowance, Class};
    use windshare_relay_core::identifiers::ShareId;

    let now = crate::node::now().unsigned_abs();
    let state = Arc::new(Mutex::new(FakeAuthority::new(now, 100, false)));
    let (url, server) = spawn_fake_authority(Arc::clone(&state)).await;
    let directory = tempfile::tempdir().unwrap();
    let (reporter, store, attribution) = {
        let authority = state.lock().unwrap();
        cycle_reporter(directory.path(), &authority, url)
    };
    let _resident = reporter
        .node
        .access
        .reserve(ShareId::new([7; 12]).unwrap(), attribution)
        .unwrap();
    reporter.node.usage.record(attribution, 100, now);

    let (versions, updates) = tokio::sync::watch::channel(0);
    reporter.run_report_cycle(&versions, now).await;
    server.abort();

    let order = state.lock().unwrap().order.clone();
    assert_eq!(order, vec!["usage", "status"]);
    assert_eq!(
        reporter.node.access.class(attribution, now),
        (Class::Lowest, Some(Allowance::Spent))
    );
    assert_eq!(*updates.borrow(), 3);
    let issuer = reporter.authority.issuer().unwrap();
    let node = reporter.signing_key.verifying_key().to_bytes();
    assert!(store.next_usage_batch(issuer, node).unwrap().is_none());
}

#[tokio::test]
async fn report_cycle_still_refreshes_status_when_usage_retry_fails() {
    use windshare_relay_access::entitlement::{Allowance, Class};
    use windshare_relay_core::identifiers::ShareId;

    let now = crate::node::now().unsigned_abs();
    let state = Arc::new(Mutex::new(FakeAuthority::new(now, 100, true)));
    let (url, server) = spawn_fake_authority(Arc::clone(&state)).await;
    let directory = tempfile::tempdir().unwrap();
    let (reporter, store, attribution) = {
        let authority = state.lock().unwrap();
        cycle_reporter(directory.path(), &authority, url)
    };
    let _resident = reporter
        .node
        .access
        .reserve(ShareId::new([8; 12]).unwrap(), attribution)
        .unwrap();
    reporter.node.usage.record(attribution, 50, now);

    let (versions, updates) = tokio::sync::watch::channel(0);
    reporter.run_report_cycle(&versions, now).await;
    server.abort();

    let order = state.lock().unwrap().order.clone();
    assert_eq!(order, vec!["usage", "status"]);
    assert_eq!(
        reporter.node.access.class(attribution, now),
        (Class::Standard, Some(Allowance::Available))
    );
    assert_eq!(*updates.borrow(), 3);
    let issuer = reporter.authority.issuer().unwrap();
    let node = reporter.signing_key.verifying_key().to_bytes();
    assert!(store.next_usage_batch(issuer, node).unwrap().is_some());
}
