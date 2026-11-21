use super::*;
use crate::telemetry::{JsonSubscriber, format_event, parse_level};
use serde_json::Value;
use std::{
    cell::RefCell,
    sync::{
        Arc, Mutex, Once,
        atomic::{AtomicBool, Ordering},
    },
};
use tracing::{
    Event, Level, Metadata, Subscriber, level_filters::LevelFilter, span, subscriber::Interest,
};
use windshare_relay_access::restrictions::Restriction;

type RecordedEvent = (Level, Value);
type CaptureBuffer = Arc<Mutex<Vec<RecordedEvent>>>;

thread_local! {
    static CAPTURE: RefCell<Option<CaptureBuffer>> = const { RefCell::new(None) };
}

struct CaptureSubscriber {
    ready: Arc<AtomicBool>,
}

impl Subscriber for CaptureSubscriber {
    fn register_callsite(&self, _metadata: &'static Metadata<'static>) -> Interest {
        Interest::sometimes()
    }

    fn max_level_hint(&self) -> Option<LevelFilter> {
        Some(if self.ready.load(Ordering::Acquire) {
            LevelFilter::TRACE
        } else {
            LevelFilter::OFF
        })
    }

    fn enabled(&self, _metadata: &Metadata<'_>) -> bool {
        CAPTURE.with(|slot| slot.borrow().is_some())
    }

    fn new_span(&self, _span: &span::Attributes<'_>) -> span::Id {
        span::Id::from_u64(1)
    }

    fn record(&self, _span: &span::Id, _values: &span::Record<'_>) {}

    fn record_follows_from(&self, _span: &span::Id, _follows: &span::Id) {}

    fn event(&self, event: &Event<'_>) {
        CAPTURE.with(|slot| {
            let Some(buffer) = slot.borrow().as_ref().cloned() else {
                return;
            };
            let parsed: Value = serde_json::from_str(&format_event(event)).unwrap();
            buffer
                .lock()
                .unwrap()
                .push((*event.metadata().level(), parsed));
        });
    }

    fn enter(&self, _span: &span::Id) {}

    fn exit(&self, _span: &span::Id) {}
}

fn install_capture_subscriber() {
    static INIT: Once = Once::new();
    INIT.call_once(|| {
        // Keep max level OFF until publication finishes so parallel test threads
        // cannot poison callsites against NoSubscriber during initialization.
        let ready = Arc::new(AtomicBool::new(false));
        let dispatch = tracing::Dispatch::new(CaptureSubscriber {
            ready: Arc::clone(&ready),
        });
        tracing::dispatcher::set_global_default(dispatch)
            .expect("authority observability test subscriber must be sole global default");
        ready.store(true, Ordering::SeqCst);
        tracing::callsite::rebuild_interest_cache();
    });
}

struct CaptureGuard(Option<CaptureBuffer>);

impl CaptureGuard {
    fn enter(buffer: CaptureBuffer) -> Self {
        Self(CAPTURE.with(|slot| slot.replace(Some(buffer))))
    }
}

impl Drop for CaptureGuard {
    fn drop(&mut self) {
        let _ = CAPTURE.try_with(|slot| slot.replace(self.0.take()));
    }
}

fn capture_events(exercise: impl FnOnce()) -> Vec<RecordedEvent> {
    install_capture_subscriber();
    let buffer = CaptureBuffer::default();
    let guard = CaptureGuard::enter(Arc::clone(&buffer));
    exercise();
    drop(guard);
    buffer.lock().unwrap().clone()
}

fn events_named<'a>(events: &'a [RecordedEvent], name: &str) -> Vec<&'a Value> {
    events
        .iter()
        .filter(|(_, entry)| entry["fields"]["event"] == name)
        .map(|(_, entry)| entry)
        .collect()
}

#[test]
fn telemetry_level_parser_defaults_to_info_and_honors_explicit_levels() {
    assert_eq!(parse_level(None), LevelFilter::INFO);
    assert_eq!(parse_level(Some("")), LevelFilter::INFO);
    assert_eq!(parse_level(Some("verbose")), LevelFilter::INFO);
    assert_eq!(parse_level(Some("info")), LevelFilter::INFO);
    assert_eq!(parse_level(Some(" OFF ")), LevelFilter::OFF);
    assert_eq!(parse_level(Some("Error")), LevelFilter::ERROR);
    assert_eq!(parse_level(Some("warn")), LevelFilter::WARN);
    assert_eq!(parse_level(Some("DeBuG")), LevelFilter::DEBUG);
    assert_eq!(parse_level(Some("trace")), LevelFilter::TRACE);
}

#[test]
fn json_subscriber_formats_all_field_kinds_and_respects_max_level() {
    install_capture_subscriber();
    let emitted = Arc::new(Mutex::new(Vec::<(Level, String)>::new()));
    let sink = Arc::clone(&emitted);
    let dispatch = tracing::Dispatch::new(JsonSubscriber::new(
        LevelFilter::INFO,
        move |level, line| {
            sink.lock().unwrap().push((level, line));
        },
    ));

    tracing::dispatcher::with_default(&dispatch, || {
        let span = tracing::info_span!("ignored_span", k = "v");
        let _enter = span.enter();
        span.record("k", "next");
        let io_error = std::io::Error::other("disk full");
        tracing::info!(
            event = "sample_info",
            signed = -7_i64,
            unsigned = 42_u64,
            big_signed = -9_i128,
            big_unsigned = 11_u128,
            ratio = 1.5_f64,
            invalid_float = f64::NAN,
            flag = true,
            err = &io_error as &dyn std::error::Error,
            debug_val = ?RegistrationMode::ProofOfWork,
        );
        tracing::debug!(event = "filtered_debug");
        tracing::warn!(event = "sample_warn");
    });

    let records = emitted.lock().unwrap();
    assert_eq!(records.len(), 2);
    assert_eq!(records[0].0, Level::INFO);
    let first: Value = serde_json::from_str(&records[0].1).unwrap();
    assert_eq!(first["level"], "INFO");
    assert_eq!(first["fields"]["event"], "sample_info");
    assert_eq!(first["fields"]["signed"], -7);
    assert_eq!(first["fields"]["unsigned"], 42);
    assert_eq!(first["fields"]["big_signed"], "-9");
    assert_eq!(first["fields"]["big_unsigned"], "11");
    assert_eq!(first["fields"]["ratio"], 1.5);
    assert!(first["fields"]["invalid_float"].is_null());
    assert_eq!(first["fields"]["flag"], true);
    assert_eq!(first["fields"]["err"], "disk full");
    assert_eq!(first["fields"]["debug_val"], "ProofOfWork");
    assert_eq!(records[1].0, Level::WARN);

    let off_emitted = Arc::new(Mutex::new(Vec::<String>::new()));
    let off_sink = Arc::clone(&off_emitted);
    let off_dispatch =
        tracing::Dispatch::new(JsonSubscriber::new(LevelFilter::OFF, move |_, line| {
            off_sink.lock().unwrap().push(line);
        }));
    tracing::dispatcher::with_default(&off_dispatch, || {
        tracing::error!(event = "suppressed_error");
    });
    assert!(off_emitted.lock().unwrap().is_empty());
}

#[test]
fn registration_traces_record_attempt_fingerprint_and_rate_limit_escalation() {
    let mut authority = fixture();
    let settings = Settings {
        registration: RegistrationPolicy::Automatic,
        source_rate: Rate {
            burst: 1,
            refill_seconds: 60,
        },
        global_rate: Rate {
            burst: 2,
            refill_seconds: 60,
        },
        ..Settings::default()
    };
    save(&mut authority.store, "settings", &settings).unwrap();

    let first_request = registration(1);
    let raw_secret = first_request.client_secret.clone();
    let raw_digest = URL_SAFE_NO_PAD.encode(crate::registration::digest(&raw_secret));

    let events = capture_events(|| {
        authority.register("src-a", first_request).unwrap();
        assert!(authority.register("src-a", registration(2)).is_err());
        assert!(authority.register("src-a", registration(2)).is_err());
    });

    let serialized_first = format!("{events:?}");
    assert!(!serialized_first.contains("src-a"));

    let created = events_named(&events, "account_created");
    assert_eq!(created.len(), 1);
    let attempt = created[0]["fields"]["attempt"].as_str().unwrap();
    assert_ne!(attempt, "");
    assert_ne!(attempt, raw_secret);
    assert_ne!(attempt, raw_digest);
    assert_eq!(created[0]["fields"]["mode"], "Automatic");
    let source_bucket = created[0]["fields"]["source_bucket"].as_str().unwrap();
    assert!(source_bucket.starts_with("creation-source/"));

    let rate_limited = events_named(&events, "registration_rate_limited");
    assert_eq!(rate_limited.len(), 2);
    assert_eq!(rate_limited[0]["fields"]["reason"], "source_rate_limited");
    assert_eq!(rate_limited[0]["fields"]["bursts"], 1);
    assert_eq!(rate_limited[0]["fields"]["source_bucket"], source_bucket);
    assert_eq!(
        rate_limited[0]["fields"]["attempt"],
        rate_limited[1]["fields"]["attempt"]
    );

    let mut authority = fixture();
    let settings = Settings {
        registration: RegistrationPolicy::Automatic,
        source_rate: Rate {
            burst: 2,
            refill_seconds: 60,
        },
        global_rate: Rate {
            burst: 2,
            refill_seconds: 60,
        },
        ..Settings::default()
    };
    save(&mut authority.store, "settings", &settings).unwrap();
    let events = capture_events(|| {
        authority.register("src-a", registration(1)).unwrap();
        authority.register("src-a", registration(2)).unwrap();
        // Both local and global credits are now 0; two refusals push bursts to 2.
        assert!(authority.register("src-a", registration(3)).is_err());
        assert!(authority.register("src-a", registration(3)).is_err());
        // Different source hits global only.
        assert!(authority.register("src-b", registration(4)).is_err());
        // Partial refill restores 1 of 2 credit so bursts stays >= 2 -> PoW escalation.
        authority.environment.now += 60;
        assert!(authority.register("src-a", registration(3)).is_err());
    });

    let serialized_second = format!("{events:?}");
    assert!(!serialized_second.contains("src-a"));
    assert!(!serialized_second.contains("src-b"));

    let limited = events_named(&events, "registration_rate_limited");
    assert!(
        limited
            .iter()
            .any(|e| e["fields"]["reason"] == "source_and_global_rate_limited")
    );
    assert!(
        limited
            .iter()
            .any(|e| e["fields"]["reason"] == "global_rate_limited")
    );

    let verification = events_named(&events, "registration_verification_required");
    assert!(verification.iter().any(|e| {
        e["fields"]["mode"] == "ProofOfWork"
            && e["fields"]["reason"] == "source_burst"
            && e["fields"]["source_bucket"] == source_bucket
    }));
}

#[test]
fn email_registration_and_recovery_traces_record_mode_and_attempt() {
    let mut authority = fixture();
    authority.environment.registration_email_configured = true;
    save(
        &mut authority.store,
        "settings",
        &Settings {
            registration: RegistrationPolicy::Email,
            ..Settings::default()
        },
    )
    .unwrap();
    let mailbox = Mailbox::parse("alice@gmail.com").unwrap();
    let first = registration(10);
    let (challenge, code) = authority
        .deliver_email("src-email", registration_email(&first), &mailbox)
        .unwrap();
    authority
        .register(
            "src-email",
            Registration {
                challenge: Some(challenge.id),
                solution: Some(code),
                ..first
            },
        )
        .unwrap();

    let recovery = registration(11);
    let (challenge, code) = authority
        .deliver_email("src-email", registration_email(&recovery), &mailbox)
        .unwrap();
    let events = capture_events(|| {
        assert!(authority.register("src-email", registration(12)).is_err());
        authority
            .register(
                "src-email",
                Registration {
                    challenge: Some(challenge.id),
                    solution: Some(code),
                    ..recovery
                },
            )
            .unwrap();
    });
    let serialized_email = format!("{events:?}");
    assert!(!serialized_email.contains("src-email"));

    let verification = events_named(&events, "registration_verification_required");
    assert_eq!(verification.len(), 1);
    assert_eq!(verification[0]["fields"]["mode"], "Email");
    assert_eq!(verification[0]["fields"]["reason"], "policy");
    let email_source_bucket = verification[0]["fields"]["source_bucket"].as_str().unwrap();
    assert!(email_source_bucket.starts_with("creation-source/"));

    let recovered = events_named(&events, "account_recovered_by_email");
    assert_eq!(recovered.len(), 1);
    assert_eq!(recovered[0]["fields"]["mode"], "Email");
    assert_eq!(recovered[0]["fields"]["source_bucket"], email_source_bucket);
    assert_ne!(recovered[0]["fields"]["attempt"].as_str().unwrap(), "");
}

#[test]
fn ticket_auth_refusals_and_node_status_batches_emit_structured_traces() {
    let mut authority = fixture();
    let session = authority.register("source", registration(1)).unwrap();
    let secret = URL_SAFE_NO_PAD.encode([1; 32]);
    let bad_secret = "not-a-valid-credential-secret-value";

    // Block account, revoke a second credential, and delete a third account to
    // cover all four refusal paths without leaking raw credential secrets.
    let sibling_secret = URL_SAFE_NO_PAD.encode([2; 32]);
    let sibling = Credential {
        id: [88; 16],
        account: session.account,
        secret_digest: crate::registration::digest(&sibling_secret),
        created_at: authority.environment.now(),
        revoked: true,
    };
    save(
        &mut authority.store,
        &key("credential", &sibling.secret_digest),
        &sibling,
    )
    .unwrap();

    let orphan_secret = URL_SAFE_NO_PAD.encode([3; 32]);
    let orphan = Credential {
        id: [89; 16],
        account: [99; 16],
        secret_digest: crate::registration::digest(&orphan_secret),
        created_at: authority.environment.now(),
        revoked: false,
    };
    save(
        &mut authority.store,
        &key("credential", &orphan.secret_digest),
        &orphan,
    )
    .unwrap();

    let node = SigningKey::from_bytes(&[7; 32]);
    save(
        &mut authority.store,
        &key("node", &node.verifying_key().to_bytes()),
        &Node {
            version: 3,
            key: node.verifying_key().to_bytes(),
            zone: Zone::Global,
            region: "test".into(),
            anonymous: true,
            enabled: true,
        },
    )
    .unwrap();
    let status_batch = batch(&authority, &node, session.account, 5, 2, 0);

    let events = capture_events(|| {
        assert!(authority.refresh(bad_secret).is_err());
        assert!(authority.refresh(&sibling_secret).is_err());
        assert!(authority.refresh(&orphan_secret).is_err());
        authority
            .set_restriction(Restriction::Account(session.account), true)
            .unwrap();
        assert!(authority.refresh(&secret).is_err());
        let _ = status(&mut authority, &status_batch);
    });

    let serialized_all = format!("{events:?}");
    assert!(!serialized_all.contains(bad_secret));
    assert!(!serialized_all.contains(&secret));
    assert!(!serialized_all.contains(&sibling_secret));

    let refused = events_named(&events, "account_auth_refused");
    let reasons: Vec<_> = refused
        .iter()
        .map(|entry| entry["fields"]["reason"].as_str().unwrap())
        .collect();
    assert_eq!(
        reasons,
        vec![
            "credentials",
            "credential_revoked",
            "unknown_account",
            "account_blocked",
        ]
    );

    let status_events = events_named(&events, "node_status_served");
    assert_eq!(status_events.len(), 1);
    assert_eq!(status_events[0]["fields"]["entitlements"], 1);
    assert_eq!(status_events[0]["fields"]["policy_version"], 3);
}

#[test]
fn ticket_issuance_and_governance_traces_expose_expiry_and_target_fields() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    let secret = URL_SAFE_NO_PAD.encode([1; 32]);
    let share = [9; 12];

    let mut refreshed_entitlement = None;
    let events = capture_events(|| {
        let session = authority.register("source", registration(1)).unwrap();
        let refreshed = authority.refresh(&secret).unwrap();
        assert!(refreshed.entitlement.valid_until > 0);
        refreshed_entitlement = Some(refreshed.entitlement);
        authority
            .block(
                &admin,
                Block::Share {
                    id: share,
                    blocked: true,
                },
            )
            .unwrap();
        authority
            .block(
                &admin,
                Block::Account {
                    id: session.account,
                    blocked: true,
                },
            )
            .unwrap();
        authority
            .report("source", share, "abuse report".into())
            .unwrap();
    });

    let refreshed_entitlement = refreshed_entitlement.unwrap();
    let issued = events_named(&events, "account_ticket_issued");
    assert_eq!(issued.len(), 2);
    assert_eq!(
        issued[1]["fields"]["valid_until"],
        refreshed_entitlement.valid_until
    );
    assert_eq!(
        issued[1]["fields"]["version"],
        refreshed_entitlement.version
    );

    let blocks = events_named(&events, "access_block_changed");
    assert_eq!(blocks.len(), 2);
    assert_eq!(blocks[0]["fields"]["kind"], "share");
    assert!(blocks[0]["fields"].get("share").is_some());
    assert!(blocks[0]["fields"].get("account").is_none());
    assert_eq!(blocks[0]["fields"]["blocked"], true);
    assert_eq!(blocks[1]["fields"]["kind"], "account");
    assert!(blocks[1]["fields"].get("account").is_some());
    assert!(blocks[1]["fields"].get("share").is_none());
    assert_eq!(blocks[1]["fields"]["blocked"], true);

    let reported = events_named(&events, "share_reported");
    assert_eq!(reported.len(), 1);
    assert!(reported[0]["fields"].get("share").is_some());
    assert!(reported[0]["fields"].get("report").is_some());
}

#[test]
fn signing_issuer_mismatch_emits_structured_error_trace() {
    let mut authority = fixture();
    let now = authority.environment.now();
    let expected = URL_SAFE_NO_PAD.encode(authority.signing.generation.issuer);
    let foreign = signing_fixture_for_root([99; 32], 2);
    let configured = URL_SAFE_NO_PAD.encode(foreign.generation.issuer);

    let events = capture_events(|| {
        assert!(foreign.activate(&mut authority.store, now).is_err());
    });

    let mismatched: Vec<_> = events
        .iter()
        .filter(|(level, entry)| {
            *level == Level::ERROR
                && entry["fields"]["event"] == "authority_signing_issuer_mismatch"
        })
        .map(|(_, entry)| entry)
        .collect();
    assert_eq!(mismatched.len(), 1);
    assert_eq!(mismatched[0]["fields"]["expected_issuer"], expected);
    assert_eq!(mismatched[0]["fields"]["configured_issuer"], configured);
    assert_eq!(mismatched[0]["fields"]["previous_generation"], 1);
    assert_eq!(mismatched[0]["fields"]["configured_generation"], 2);
}
