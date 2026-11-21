use super::*;
use crate::{
    fleet::signature::MAX_BYTES,
    public_directory::{DirectoryCache, VerifiedMetadata},
};
use chrono::{DateTime, Utc};
use std::cell::Cell;

struct Fixture {
    release: Release,
    online: SigningKey,
    cert: Value,
    bundled_body: String,
}

impl Fixture {
    fn new() -> Self {
        let root = SigningKey::from_bytes(&[15; 32]);
        let online = SigningKey::from_bytes(&[16; 32]);
        let mut cert = signed(
            &root,
            b"windshare/network-certificate/v1\0",
            &json!({
                "format": 1,
                "network": "test",
                "generation": 1,
                "key": URL_SAFE_NO_PAD.encode(online.verifying_key().to_bytes()),
                "not_before": "1970-01-01T00:00:00Z",
                "expires_at": "1970-05-01T00:00:00Z",
            }),
        );
        cert["root"] = json!("test-root");
        let bundled = Self::sign_envelope(&online, &cert, "bundled", 4, 0, 10 * 86_400);
        let release: Release = serde_json::from_value(json!({
            "network": "test",
            "roots": [{
                "id": "test-root",
                "key": URL_SAFE_NO_PAD.encode(root.verifying_key().to_bytes()),
            }],
            "snapshot": bundled,
            "link_host": "https://ws.ourworld.top",
            "backup_entry": "https://windshare.cc",
        }))
        .unwrap();
        let bundled_body = serde_json::to_string(&release.snapshot).unwrap();
        Self {
            release,
            online,
            cert,
            bundled_body,
        }
    }

    fn sign_envelope(
        online: &SigningKey,
        cert: &Value,
        alias: &str,
        serial: u64,
        issued_at: i64,
        expires_at: i64,
    ) -> Value {
        let mut entry = spec(alias, 9);
        entry["relay"] = json!(format!("https://{alias}.example/"));
        entry["status"] = json!("active");
        let issued = DateTime::<Utc>::from_timestamp(issued_at, 0)
            .unwrap()
            .to_rfc3339();
        let expires = DateTime::<Utc>::from_timestamp(expires_at, 0)
            .unwrap()
            .to_rfc3339();
        let mut envelope = signed(
            online,
            b"windshare/network-directory/v1\0",
            &json!({
                "format": 1,
                "network": "test",
                "generation": 1,
                "serial": serial,
                "issued_at": issued,
                "expires_at": expires,
                "nodes": [entry],
                "public_stun": [],
            }),
        );
        envelope["certificate"] = cert.clone();
        envelope
    }

    fn copy(&self, alias: &str, serial: u64, issued_at: i64, expires_at: i64) -> String {
        let envelope: Envelope = serde_json::from_value(Self::sign_envelope(
            &self.online,
            &self.cert,
            alias,
            serial,
            issued_at,
            expires_at,
        ))
        .unwrap();
        serde_json::to_string(&envelope).unwrap()
    }
}

fn select_counted(
    cache: &mut DirectoryCache,
    release: &Release,
    body: Option<String>,
    now: i64,
    verifications: &Cell<usize>,
) -> Option<String> {
    cache
        .select_with(release, body, now, &mut |raw, trust| {
            verifications.set(verifications.get() + 1);
            VerifiedMetadata::verify(raw, trust)
        })
        .unwrap()
        .map(|copy| copy.body.clone())
}

#[test]
fn stale_and_reformatted_kv_copies_verify_once_and_emit_no_warnings() {
    let fixture = Fixture::new();
    let stale = fixture.copy("stale", 3, 0, 10 * 86_400);
    let reformatted = format!("  {}  ", fixture.bundled_body);
    let mut cache = DirectoryCache::new();
    let verifications = Cell::new(0);

    let events = super::super::observability::capture_events(|| {
        // Cold start verifies the bundled release and the stale KV body once.
        let first = select_counted(
            &mut cache,
            &fixture.release,
            Some(stale.clone()),
            1_000,
            &verifications,
        );
        assert_eq!(first.as_deref(), Some(fixture.bundled_body.as_str()));
        assert_eq!(verifications.get(), 2);

        // Repeated reads of the same stale KV body skip verification completely.
        for _ in 0..3 {
            let repeated = select_counted(
                &mut cache,
                &fixture.release,
                Some(stale.clone()),
                1_000,
                &verifications,
            );
            assert_eq!(repeated.as_deref(), Some(fixture.bundled_body.as_str()));
        }
        assert_eq!(verifications.get(), 2);

        // Same version with different JSON whitespace verifies once and does not warn.
        let first_reformatted = select_counted(
            &mut cache,
            &fixture.release,
            Some(reformatted.clone()),
            1_000,
            &verifications,
        );
        assert_eq!(
            first_reformatted.as_deref(),
            Some(fixture.bundled_body.as_str())
        );
        assert_eq!(verifications.get(), 3);

        let second_reformatted = select_counted(
            &mut cache,
            &fixture.release,
            Some(reformatted),
            1_000,
            &verifications,
        );
        assert_eq!(
            second_reformatted.as_deref(),
            Some(fixture.bundled_body.as_str())
        );
        assert_eq!(verifications.get(), 3);
    });

    assert_eq!(
        super::super::observability::events_named(&events, "directory_copy_rejected"),
        Vec::<&Value>::new()
    );
}

#[test]
fn future_dated_and_expired_kv_copies_verify_once_and_respect_validity_window() {
    let fixture = Fixture::new();
    let issued_at = 10_000;
    let active_at = issued_at - 300;
    let future = fixture.copy("future", 5, issued_at, issued_at + 10 * 86_400);
    let expired = fixture.copy("expired", 6, 0, 1_000);
    let expired_at = 1_000 + 7 * 86_400;
    let mut cache = DirectoryCache::new();
    let verifications = Cell::new(0);

    let events = super::super::observability::capture_events(|| {
        // Before `start`, future copy verifies once and falls back to bundled.
        assert_eq!(
            select_counted(
                &mut cache,
                &fixture.release,
                Some(future.clone()),
                active_at - 10,
                &verifications,
            )
            .as_deref(),
            Some(fixture.bundled_body.as_str())
        );
        assert_eq!(verifications.get(), 2);

        // Still before `start`: served from memoized KV without re-verifying.
        assert_eq!(
            select_counted(
                &mut cache,
                &fixture.release,
                Some(future.clone()),
                active_at - 1,
                &verifications,
            )
            .as_deref(),
            Some(fixture.bundled_body.as_str())
        );
        assert_eq!(verifications.get(), 2);

        // Once `now` reaches `start`, the memoized copy promotes without re-verifying.
        assert_eq!(
            select_counted(
                &mut cache,
                &fixture.release,
                Some(future.clone()),
                active_at,
                &verifications,
            )
            .as_deref(),
            Some(future.as_str())
        );
        assert_eq!(verifications.get(), 2);

        // Steady-state fast path now matches `latest.body`.
        assert_eq!(
            select_counted(
                &mut cache,
                &fixture.release,
                Some(future.clone()),
                active_at + 5,
                &verifications,
            )
            .as_deref(),
            Some(future.as_str())
        );
        assert_eq!(verifications.get(), 2);

        // Already-expired newer copy verifies once and stays rejected without re-verifying.
        for _ in 0..3 {
            assert_eq!(
                select_counted(
                    &mut cache,
                    &fixture.release,
                    Some(expired.clone()),
                    expired_at,
                    &verifications,
                )
                .as_deref(),
                Some(future.as_str())
            );
        }
        assert_eq!(verifications.get(), 3);
    });

    assert_eq!(
        super::super::observability::events_named(&events, "directory_copy_rejected"),
        Vec::<&Value>::new()
    );
}

#[test]
fn invalid_and_oversized_kv_copies_warn_only_on_change() {
    let fixture = Fixture::new();
    let invalid_a = "{not-json".to_owned();
    let invalid_b = "{\"payload\":\"bad\"}".to_owned();
    let oversized = " ".repeat(MAX_BYTES + 1);
    let mut cache = DirectoryCache::new();
    let verifications = Cell::new(0);

    let events = super::super::observability::capture_events(|| {
        assert_eq!(
            select_counted(&mut cache, &fixture.release, None, 1_000, &verifications).as_deref(),
            Some(fixture.bundled_body.as_str())
        );
        assert_eq!(verifications.get(), 1);

        // Repeated invalid payload warns once and verifies once.
        for _ in 0..3 {
            assert_eq!(
                select_counted(
                    &mut cache,
                    &fixture.release,
                    Some(invalid_a.clone()),
                    1_000,
                    &verifications,
                )
                .as_deref(),
                Some(fixture.bundled_body.as_str())
            );
        }
        assert_eq!(verifications.get(), 2);

        // A different invalid payload re-evaluates and logs once.
        assert_eq!(
            select_counted(
                &mut cache,
                &fixture.release,
                Some(invalid_b),
                1_000,
                &verifications,
            )
            .as_deref(),
            Some(fixture.bundled_body.as_str())
        );
        assert_eq!(verifications.get(), 3);

        // Oversized payload warns once and suppresses repeats without retaining the body.
        for _ in 0..3 {
            assert_eq!(
                select_counted(
                    &mut cache,
                    &fixture.release,
                    Some(oversized.clone()),
                    1_000,
                    &verifications,
                )
                .as_deref(),
                Some(fixture.bundled_body.as_str())
            );
        }
        assert_eq!(verifications.get(), 4);

        // Recovery to `latest.body` clears rejected KV memory, so a later regression logs again.
        assert_eq!(
            select_counted(
                &mut cache,
                &fixture.release,
                Some(fixture.bundled_body.clone()),
                1_000,
                &verifications,
            )
            .as_deref(),
            Some(fixture.bundled_body.as_str())
        );
        assert_eq!(verifications.get(), 4);

        assert_eq!(
            select_counted(
                &mut cache,
                &fixture.release,
                Some(invalid_a),
                1_000,
                &verifications,
            )
            .as_deref(),
            Some(fixture.bundled_body.as_str())
        );
        assert_eq!(verifications.get(), 5);
    });

    let rejected = super::super::observability::events_named(&events, "directory_copy_rejected");
    assert_eq!(rejected.len(), 4);
    assert!(
        rejected[2]["fields"]["error"]
            .as_str()
            .is_some_and(|message| message.contains("directory too large"))
    );
}

#[test]
fn equivocating_kv_copy_warns_once_and_recovers_on_newer_revision() {
    let fixture = Fixture::new();
    let equivocating = fixture.copy("forked", 4, 0, 10 * 86_400);
    let newer = fixture.copy("live", 5, 0, 10 * 86_400);
    let mut cache = DirectoryCache::new();
    let verifications = Cell::new(0);

    let events = super::super::observability::capture_events(|| {
        // Equivocation against `latest` warns once and suppresses repeat verification.
        for _ in 0..3 {
            assert_eq!(
                select_counted(
                    &mut cache,
                    &fixture.release,
                    Some(equivocating.clone()),
                    1_000,
                    &verifications,
                )
                .as_deref(),
                Some(fixture.bundled_body.as_str())
            );
        }
        assert_eq!(verifications.get(), 2);

        // Production `select` entry point adopts a valid newer revision cleanly.
        let adopted = cache
            .select(&fixture.release, Some(newer.clone()), 1_000)
            .unwrap()
            .unwrap();
        assert_eq!(adopted.body, newer);
    });

    let rejected = super::super::observability::events_named(&events, "directory_copy_rejected");
    assert_eq!(rejected.len(), 1);
    assert!(
        rejected[0]["fields"]["error"]
            .as_str()
            .is_some_and(|message| message.contains("directory equivocation"))
    );
}
