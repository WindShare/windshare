use super::*;
use crate::Endpoint;
use std::path::Path;
use windshare_relay_access::{entitlement::DAY_SECONDS, restrictions::Restriction};

const ISSUER: Issuer = [1; 32];
const NOW: u64 = 20 * DAY_SECONDS + 120;

fn open(path: &Path) -> Database {
    let relay = Endpoint::parse("ws://localhost:8787/v2/ws")
        .unwrap()
        .identity;
    Database::open(path, relay).unwrap()
}

fn generation(number: u64) -> Generation {
    Generation {
        issuer: ISSUER,
        number,
        key: [2; 32],
        not_before: NOW - 120,
        expires_at: NOW + 60,
    }
}

fn policy() -> NodePolicy {
    NodePolicy {
        version: 5,
        enabled: false,
        anonymous: false,
    }
}

fn entitlement(account: u8, version: u64, valid_until: u64) -> AccountEntitlement {
    AccountEntitlement {
        account: [account; 16],
        entitlement: Entitlement {
            version,
            day: NOW / DAY_SECONDS,
            valid_until,
            remaining_bytes: u64::MAX,
        },
    }
}

#[test]
fn startup_migrates_the_legacy_cache_and_preserves_durable_access_state() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("node.db");
    let db = open(&path);
    let mut restrictions = Restrictions {
        version: 255,
        ..Restrictions::default()
    };
    restrictions.insert(Restriction::Credential([3; 16]));
    let live = entitlement(4, 7, NOW + 50);
    let expired = entitlement(5, 8, NOW);
    let mut yesterday = entitlement(6, 9, NOW - 120);
    yesterday.entitlement.day -= 1;
    {
        let connection = db.0.connection.lock().unwrap();
        connection.execute_batch(
            "CREATE TABLE access_cache(singleton INTEGER PRIMARY KEY, body BLOB NOT NULL) STRICT;",
        ).unwrap();
        let entitlements =
            [&live, &expired, &yesterday].map(|value| (ISSUER, value.account, value.entitlement));
        let legacy = serde_json::json!({
            "generations": [generation(2)],
            "restrictions": [(ISSUER, &restrictions)],
            "policies": [(ISSUER, policy())],
            "entitlements": entitlements,
        });
        connection
            .execute(
                "UPDATE host SET format=?1",
                [crate::storage::LEGACY_ACCESS_CACHE_SCHEMA],
            )
            .unwrap();
        connection
            .execute(
                "INSERT INTO access_cache VALUES(1,?1)",
                [serde_json::to_vec(&legacy).unwrap()],
            )
            .unwrap();
    }

    drop(db);
    let db = open(&path);
    let restored = db.initialize_control(NOW).unwrap();
    let format: i64 =
        db.0.connection
            .lock()
            .unwrap()
            .query_row("SELECT format FROM host WHERE singleton=1", [], |row| {
                row.get(0)
            })
            .unwrap();
    assert_eq!(format, crate::storage::SCHEMA_VERSION);
    assert_eq!(
        restored.authorities,
        [AuthorityState {
            generation: generation(2),
            policy: Some(policy())
        }]
    );
    assert_eq!(restored.restrictions, [(ISSUER, restrictions.clone())]);
    let live = (
        (ISSUER, live.account),
        Quota::from_snapshot(live.entitlement, NOW),
    );
    let expired = (
        (ISSUER, expired.account),
        Quota::from_snapshot(expired.entitlement, NOW),
    );
    assert_eq!(restored.quotas.len(), 2);
    assert!(restored.quotas.contains(&live));
    assert!(restored.quotas.contains(&expired));
    assert_eq!(
        db.save_authority(generation(1)).unwrap().generation.number,
        2
    );

    let mut latest = restrictions.clone();
    latest.version += 1;
    latest.insert(Restriction::Share([4; 12]));
    db.save_restrictions(ISSUER, &latest).unwrap();
    db.save_restrictions(ISSUER, &restrictions).unwrap();
    drop(db);

    let db = open(&path);
    let restored = db.initialize_control(NOW + DAY_SECONDS).unwrap();
    assert_eq!(restored.authorities[0].generation, generation(2));
    assert_eq!(restored.authorities[0].policy, Some(policy()));
    assert_eq!(restored.restrictions, [(ISSUER, latest)]);
    assert_eq!(restored.quotas, []);
    let legacy_exists: bool =
        db.0.connection
            .lock()
            .unwrap()
            .query_row(
                "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='access_cache')",
                [],
                |row| row.get(0),
            )
            .unwrap();
    assert!(!legacy_exists);
}

#[test]
fn status_returns_only_advanced_quotas_and_expiry_survives_restart() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("node.db");
    let db = open(&path);
    db.initialize_control(NOW).unwrap();
    let authority = AuthorityState {
        generation: generation(2),
        policy: Some(policy()),
    };
    let old = entitlement(4, 1_u64 << 63, NOW + 200);
    let newer = entitlement(4, u64::MAX, NOW + 100);
    let other = entitlement(5, 1, NOW + 240);
    assert_eq!(
        db.save_status(authority, vec![old, other], NOW)
            .unwrap()
            .quotas
            .len(),
        2
    );
    let update = db.save_status(authority, vec![newer], NOW + 10).unwrap();
    assert_eq!(
        update.quotas,
        [(
            (ISSUER, newer.account),
            Quota::from_snapshot(newer.entitlement, NOW + 10)
        )]
    );
    let expired_at = NOW + 100;
    db.prune_quotas(expired_at).unwrap();
    drop(db);

    let db = open(&path);
    let restored = db.initialize_control(NOW + 25).unwrap();
    assert_eq!(restored.quotas.len(), 2);
    assert!(restored.quotas.contains(&(
        (ISSUER, newer.account),
        Quota::from_snapshot(newer.entitlement, expired_at)
    )));
    assert_eq!(
        db.save_status(authority, vec![old, newer], expired_at)
            .unwrap()
            .quotas,
        []
    );

    db.prune_quotas(NOW + DAY_SECONDS).unwrap();
    drop(db);
    let db = open(&path);
    let restored = db.initialize_control(NOW).unwrap();
    assert_eq!(restored.quota_day_floor, NOW / DAY_SECONDS + 1);
    assert_eq!(restored.quotas, []);
    assert_eq!(
        db.save_status(authority, vec![old], NOW).unwrap().quotas,
        []
    );
}
