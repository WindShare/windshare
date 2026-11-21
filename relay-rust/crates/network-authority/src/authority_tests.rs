use super::*;
#[path = "account_admin_tests.rs"]
mod account_admin;
#[path = "contract_vectors_tests.rs"]
mod contract_vectors;
#[path = "device_login_tests.rs"]
mod device_login;
#[path = "fleet/workflow_tests.rs"]
mod fleet_workflow;
#[path = "governance_tests.rs"]
mod governance;
#[path = "observability_tests.rs"]
mod observability;
#[path = "password_flow_tests.rs"]
mod password_flow;
#[path = "profile_tests.rs"]
mod profile;
#[path = "restriction_sync_tests.rs"]
mod restriction_sync;
#[path = "work_registration_tests.rs"]
mod work_registration;
use crate::{
    accounts::DailyUsageRecord,
    store::{key, load, save},
};
use anyhow::Result;
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ed25519_dalek::SigningKey;
use std::collections::BTreeMap;
use windshare_relay_access::{
    entitlement::{DAY_SECONDS, ENTITLEMENT_LIFETIME},
    ticket::Ticket,
    trust::{self, Generation, TrustStore},
    usage::{Batch, Load, RESPONSE_DOMAIN, Reply, ReportResult, USAGE_DOMAIN, UsageRecord},
};

#[derive(Clone, Default)]
struct Memory {
    records: BTreeMap<String, String>,
    put_count: usize,
}
impl Store for Memory {
    fn get(&self, key: &str) -> Result<Option<String>> {
        Ok(self.records.get(key).cloned())
    }
    fn put(&mut self, key: &str, value: &str) -> Result<()> {
        self.records.insert(key.into(), value.into());
        self.put_count += 1;
        Ok(())
    }
    fn delete(&mut self, key: &str) -> Result<()> {
        self.records.remove(key);
        Ok(())
    }
    fn prune_registration_proofs(&mut self, kind: ProofKind, now: u64) -> Result<()> {
        let prefix = format!("{}/", kind.registration_prefix());
        for (key, value) in self.scan(&prefix, None)? {
            let proof: serde_json::Value = serde_json::from_str(&value)?;
            if proof["expires_at"]
                .as_u64()
                .is_some_and(|expiry| expiry <= now)
            {
                self.delete(&key)?;
            }
        }
        Ok(())
    }
    fn scan_after(
        &self,
        prefix: &str,
        after: Option<&str>,
        limit: Option<usize>,
    ) -> Result<Vec<(String, String)>> {
        Ok(self
            .records
            .iter()
            .filter(|(key, _)| {
                key.starts_with(prefix) && after.is_none_or(|after| key.as_str() > after)
            })
            .take(limit.unwrap_or(usize::MAX))
            .map(|(key, value)| (key.clone(), value.clone()))
            .collect())
    }
    fn reports_after(&self, after: Option<ReportPosition>, limit: usize) -> Result<Vec<Report>> {
        let after = after.map(|position| (position.created_at, key("report", &position.id)));
        let mut reports = self
            .scan("report/", None)?
            .into_iter()
            .map(|(key, value)| Ok((key, serde_json::from_str::<Report>(&value)?)))
            .collect::<Result<Vec<_>>>()?;
        reports.sort_by(|(key_a, a), (key_b, b)| (b.created_at, key_b).cmp(&(a.created_at, key_a)));
        Ok(reports
            .into_iter()
            .filter(|(key, report)| {
                after
                    .as_ref()
                    .is_none_or(|(time, id)| (report.created_at, key) < (*time, id))
            })
            .take(limit)
            .map(|(_, report)| report)
            .collect())
    }
}
struct Clock {
    now: u64,
    next: u8,
    registration_email_configured: bool,
}
impl Environment for Clock {
    fn now(&self) -> u64 {
        self.now
    }
    fn registration_email_configured(&self) -> bool {
        self.registration_email_configured
    }

    fn random<const N: usize>(&mut self) -> Result<[u8; N]> {
        self.next += 1;
        Ok([self.next; N])
    }
}
// Historical enrollments had no directory metadata. Tests seed that state before exercising governance or bootstrap.
struct NodeEnrollment {
    key: [u8; 32],
    zone: Zone,
    region: String,
}
impl Authority<Memory, Clock> {
    fn enroll_node(&mut self, session: &str, record: &NodeEnrollment) -> Result<Node> {
        self.require_admin(session)?;
        let path = key("node", &record.key);
        if let Some(node) = load(&self.store, &path)? {
            return Ok(node);
        }
        let node = Node::enrolled(record.key, record.zone, record.region.clone());
        save(&mut self.store, &path, &node)?;
        Ok(node)
    }
}

fn signing_fixture(number: u64) -> Signing {
    let root = SigningKey::from_bytes(&[41; 32]);
    let key = SigningKey::from_bytes(&[42; 32]);
    let generation = Generation {
        issuer: root.verifying_key().to_bytes(),
        number,
        key: key.verifying_key().to_bytes(),
        not_before: DAY_SECONDS,
        expires_at: DAY_SECONDS * 30,
    };
    let certificate = URL_SAFE_NO_PAD.encode(generation.certify(&root).unwrap());
    Signing {
        generation,
        key,
        certificate,
    }
}
fn fixture() -> Authority<Memory, Clock> {
    Authority::new(
        Memory::default(),
        Clock {
            now: DAY_SECONDS * 2,
            next: 10,
            registration_email_configured: false,
        },
        signing_fixture(1),
    )
    .unwrap()
}
fn registration(secret: u8) -> Registration {
    Registration {
        client_secret: URL_SAFE_NO_PAD.encode([secret; 32]),
        challenge: None,
        solution: None,
        username: None,
        password: None,
    }
}

fn registration_email(request: &Registration) -> EmailVerification {
    EmailVerification::Registration {
        client_secret: request.client_secret.clone(),
    }
}

fn profile_email(bearer: &str) -> EmailVerification {
    EmailVerification::Profile {
        bearer: bearer.into(),
    }
}

impl Authority<Memory, Clock> {
    fn deliver_email(
        &mut self,
        source: &str,
        verification: EmailVerification,
        address: &Mailbox,
    ) -> Result<(Challenge, String)> {
        let delivery = self.prepare_email(source, verification, address)?;
        let code = delivery.code().to_owned();
        Ok((
            delivery.accepted(&mut self.store, self.environment.now())?,
            code,
        ))
    }

    fn hash_password(&mut self, password: &str) -> Result<HashedPassword> {
        let salt = self.environment.random::<PASSWORD_SALT_BYTES>()?;
        HashedPassword::hash(password, &salt)
    }

    fn initialize(
        &mut self,
        provided: &str,
        expected: &str,
        name: &str,
        password: &str,
    ) -> Result<String> {
        self.prepare_initialization(provided, expected, name, password.len())?;
        let hashed = self.hash_password(password)?;
        self.complete_initialization(provided, expected, name, hashed)
    }

    fn admin_login(&mut self, source: &str, name: &str, password: &str) -> Result<String> {
        let verifier = self.begin_admin_login(source, name)?;
        anyhow::ensure!(
            verifier.verify(password),
            Fault::Denied {
                reason: "credentials".into(),
            }
        );
        self.complete_admin_login(verifier.fingerprint())
    }

    fn register_with_password(
        &mut self,
        source: &str,
        mut request: Registration,
        name: &str,
        password: &str,
    ) -> Result<Session> {
        let prepare = PasswordRegistrationPrepare {
            client_secret: request.client_secret.clone(),
            challenge: request.challenge.clone(),
            solution: request.solution.clone(),
            username: name.into(),
            password_len: password.len(),
        };
        match self.prepare_password_registration(source, &prepare)? {
            PasswordRegistrationPreparation::Existing { session } => Ok(*session),
            PasswordRegistrationPreparation::Ready => {
                request.username = Some(name.into());
                request.password = Some(self.hash_password(password)?);
                self.register(source, request)
            }
        }
    }

    fn login(&mut self, source: &str, name: &str, password: &str) -> Result<Session> {
        let challenge = self.begin_account_login(source, name)?;
        anyhow::ensure!(
            challenge.password.verify(password),
            Fault::Denied {
                reason: "credentials".into(),
            }
        );
        self.complete_account_login(challenge.account, challenge.password.fingerprint())
    }

    fn set_password(
        &mut self,
        source: &str,
        secret: &str,
        name: &str,
        password: &str,
    ) -> Result<Session> {
        let prepared = self.prepare_password_change(source, secret, name, password.len())?;
        let hashed = self.hash_password(password)?;
        self.commit_password_change(secret, name, hashed, prepared.previous_verifier_digest)
    }

    fn device_sign_in(
        &mut self,
        source: &str,
        invitation: &str,
        name: &str,
        password: &str,
    ) -> Result<DeviceApproval> {
        let challenge = self.begin_device_sign_in(source, invitation, name)?;
        anyhow::ensure!(
            challenge.password.verify(password),
            Fault::Denied {
                reason: "credentials".into(),
            }
        );
        self.complete_device_sign_in(
            invitation,
            challenge.account,
            challenge.password.fingerprint(),
        )
    }
}
fn batch(
    authority: &Authority<Memory, Clock>,
    node: &SigningKey,
    account: [u8; 16],
    id: u8,
    day: u64,
    bytes: u64,
) -> Vec<u8> {
    let batch = Batch {
        id: [id; 16],
        node: node.verifying_key().to_bytes(),
        issuer: authority.signing.generation.issuer,
        created_at: authority.environment.now(),
        records: if bytes == 0 {
            vec![]
        } else {
            vec![UsageRecord {
                account,
                day,
                bytes,
            }]
        },
        resident_accounts: if bytes == 0 { vec![account] } else { vec![] },
        shares: vec![],
        load: Load {
            relay_budget: None,
            active_sessions: 1,
            wire_bytes: bytes,
            forwarded_bytes: bytes,
            admission_refusals: 0,
        },
    };
    trust::sign(USAGE_DOMAIN, &serde_json::to_vec(&batch).unwrap(), node)
}
fn reply(authority: &mut Authority<Memory, Clock>, bytes: &[u8]) -> ReportResult {
    let response = authority.usage(bytes).unwrap();
    let body = URL_SAFE_NO_PAD.decode(response.body).unwrap();
    serde_json::from_slice::<Reply<ReportResult>>(
        trust::verify(RESPONSE_DOMAIN, &body, &authority.signing.generation.key).unwrap(),
    )
    .unwrap()
    .result
}

fn status(
    authority: &mut Authority<Memory, Clock>,
    bytes: &[u8],
) -> (
    Vec<windshare_relay_access::usage::AccountEntitlement>,
    windshare_relay_access::usage::NodePolicy,
) {
    let ReportResult::Status {
        entitlements,
        policy,
        ..
    } = reply(authority, bytes)
    else {
        panic!("expected status");
    };
    (entitlements, policy)
}

#[test]
fn setup_retry_reuses_principal_and_refresh_reuses_credential() {
    let mut authority = fixture();
    let first = authority.register("source", registration(1)).unwrap();
    let writes_after_register = authority.store.put_count;
    let retry = authority.register("source", registration(1)).unwrap();
    assert_eq!(authority.store.put_count, writes_after_register);
    assert_eq!(first.account, retry.account);
    assert_eq!(first.credential, retry.credential);
    assert_eq!(
        load::<u64>(&authority.store, "account-count").unwrap(),
        Some(1)
    );
    let mut trusted = TrustStore::default();
    trusted
        .accept(authority.signing.generation, authority.environment.now())
        .unwrap();
    let ticket = Ticket::verify(
        &URL_SAFE_NO_PAD.decode(retry.ticket).unwrap(),
        &trusted,
        authority.environment.now(),
    )
    .unwrap();
    assert_eq!(ticket.credential, first.credential);
    assert_eq!(retry.entitlement, first.entitlement);
}
#[test]
fn late_reports_charge_the_original_day_and_restart_retries_do_not_double_charge() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    let session = authority.register("source", registration(1)).unwrap();
    let node = SigningKey::from_bytes(&[3; 32]);
    let node_key = node.verifying_key().to_bytes();
    save(
        &mut authority.store,
        &key("node", &node_key),
        &Node {
            listing: None,
            version: 1,
            key: node_key,
            zone: Zone::Global,
            region: "test".into(),
            anonymous: true,
            enabled: true,
        },
    )
    .unwrap();
    let settings = Settings {
        daily_bytes: 1000,
        ..Settings::default()
    };
    save(&mut authority.store, "settings", &settings).unwrap();
    let report = batch(&authority, &node, session.account, 1, 1, 700);
    assert!(matches!(
        reply(&mut authority, &report),
        ReportResult::UsageReceived
    ));
    let refresh = batch(&authority, &node, session.account, 2, 2, 0);
    let writes_before_idle = authority.store.put_count;
    let (before, _) = status(&mut authority, &refresh);
    assert_eq!(before[0].entitlement.remaining_bytes, 1000);
    assert_eq!(before[0].entitlement.version, session.entitlement.version);
    // The first node report already checkpointed load; idle refreshes write nothing.
    assert_eq!(authority.store.put_count, writes_before_idle);
    let store = authority.store.clone();
    authority = Authority::new(store, authority.environment, authority.signing).unwrap();
    assert!(matches!(
        reply(&mut authority, &report),
        ReportResult::UsageReceived
    ));
    assert_eq!(
        load::<DailyUsageRecord>(
            &authority.store,
            &format!("{}/1", key("usage", &session.account))
        )
        .unwrap(),
        Some(DailyUsageRecord {
            bytes: 700,
            version: 1,
        })
    );
    let current = batch(&authority, &node, session.account, 3, 2, 300);
    assert!(matches!(
        reply(&mut authority, &current),
        ReportResult::UsageReceived
    ));
    let (current, _) = status(&mut authority, &refresh);
    assert_eq!(current[0].entitlement.remaining_bytes, 700);
    assert!(current[0].entitlement.version > before[0].entitlement.version);
    let writes_after_charge = authority.store.put_count;
    authority.environment.now += ENTITLEMENT_LIFETIME - 1;
    let refresh = batch(&authority, &node, session.account, 4, 2, 0);
    let (fresh, _) = status(&mut authority, &refresh);
    assert_eq!(authority.store.put_count, writes_after_charge);
    assert_eq!(fresh[0].entitlement.version, current[0].entitlement.version);
    assert!(fresh[0].entitlement.valid_until > current[0].entitlement.valid_until);
    assert_eq!(
        authority
            .node_by_key(&admin, &node_key)
            .unwrap()
            .report
            .unwrap()
            .reported_at,
        Some(authority.environment.now)
    );

    // Changing the daily allowance limit advances every account's state version immediately.
    authority
        .update_settings(
            &admin,
            &SettingsChanges {
                daily_bytes: Some(2000),
                ..SettingsChanges::default()
            },
        )
        .unwrap();
    let refresh = batch(&authority, &node, session.account, 5, 2, 0);
    let (raised, _) = status(&mut authority, &refresh);
    assert_eq!(raised[0].entitlement.remaining_bytes, 1700);
    assert!(raised[0].entitlement.version > fresh[0].entitlement.version);

    // Advancing past the load checkpoint interval persists a single node-load row.
    let writes_before_checkpoint = authority.store.put_count;
    authority.environment.now += crate::nodes::NODE_LOAD_CHECKPOINT_SECONDS;
    let refresh = batch(&authority, &node, session.account, 6, 2, 0);
    let _ = status(&mut authority, &refresh);
    assert_eq!(authority.store.put_count, writes_before_checkpoint + 1);
}
#[test]
fn credential_revocation_preserves_siblings_and_is_not_reset_by_new_days() {
    let mut authority = fixture();
    let first = authority.register("source", registration(1)).unwrap();
    let second = Credential {
        id: [97; 16],
        account: first.account,
        secret_digest: crate::registration::digest("second"),
        created_at: authority.environment.now(),
        revoked: false,
    };
    save(
        &mut authority.store,
        &key("credential", &second.secret_digest),
        &second,
    )
    .unwrap();
    save(
        &mut authority.store,
        &key("credential-id", &second.id),
        &second.secret_digest,
    )
    .unwrap();
    authority
        .revoke_credential(&URL_SAFE_NO_PAD.encode([1; 32]), second.id)
        .unwrap();
    authority.environment.now += DAY_SECONDS;
    assert!(authority.refresh("second").is_err());
    assert!(authority.refresh(&URL_SAFE_NO_PAD.encode([1; 32])).is_ok());
    assert!(
        authority
            .restricted(windshare_relay_access::restrictions::Restriction::Credential(second.id))
            .unwrap()
    );
}
#[test]
fn creation_bursts_do_not_limit_existing_accounts_and_a_fixed_mode_survives_a_milestone() {
    let mut authority = fixture();
    let settings = Settings {
        registration: RegistrationPolicy::Automatic,
        source_rate: Rate {
            burst: 1,
            refill_seconds: 60,
        },
        ..Settings::default()
    };
    save(&mut authority.store, "settings", &settings).unwrap();
    save(&mut authority.store, "account-count", &999u64).unwrap();
    authority.register("source", registration(1)).unwrap();
    assert_eq!(
        authority.settings().unwrap().registration,
        RegistrationPolicy::Automatic
    );
    assert!(authority.store.get("review/1000").unwrap().is_some());
    assert!(
        authority
            .register("source", registration(2))
            .unwrap_err()
            .downcast_ref::<Fault>()
            .is_some_and(|fault| matches!(fault, Fault::RateLimited { .. }))
    );
    assert!(authority.refresh(&URL_SAFE_NO_PAD.encode([1; 32])).is_ok());
    authority.environment.now += 60;
    assert!(authority.register("source", registration(2)).is_ok());
}
#[test]
fn new_account_requirements_follow_milestones_and_available_email_delivery() {
    let mut authority = fixture();
    save(&mut authority.store, "account-count", &999u64).unwrap();
    let first = authority.register("source", registration(1)).unwrap();
    assert_eq!(authority.account_count().unwrap(), 1_000);

    let attempt = |expected, authority: &mut Authority<Memory, Clock>| {
        let error = authority.register("source", registration(2)).unwrap_err();
        let Fault::Verification { challenge } = error.downcast::<Fault>().unwrap() else {
            panic!("expected a registration challenge");
        };
        assert_eq!(challenge.mode, expected);
    };
    attempt(RegistrationMode::ProofOfWork, &mut authority);
    assert_eq!(authority.account_count().unwrap(), 1_000);

    save(&mut authority.store, "account-count", &4_000u64).unwrap();
    attempt(RegistrationMode::ProofOfWork, &mut authority);
    authority.environment.registration_email_configured = true;
    attempt(RegistrationMode::Email, &mut authority);
    assert_eq!(authority.account_count().unwrap(), 4_000);

    let retry = authority.register("source", registration(1)).unwrap();
    assert_eq!(retry.credential, first.credential);
    assert_eq!(authority.account_count().unwrap(), 4_000);
}
#[test]
fn signing_activation_writes_only_when_generation_changes() {
    let mut authority = fixture();
    let now = authority.environment.now();
    let signing = &authority.signing;
    let store = &mut authority.store;
    assert_eq!(store.put_count, 1);

    signing.activate(store, now).unwrap();
    assert_eq!(store.put_count, 1);
    assert!(
        signing
            .activate(store, signing.generation.expires_at)
            .is_err()
    );
    assert_eq!(store.put_count, 1);

    let next = signing_fixture(2);
    next.activate(store, now).unwrap();
    assert_eq!(store.put_count, 2);
    assert!(signing.activate(store, now).is_err());
    assert_eq!(store.put_count, 2);
}

#[test]
fn initialization_is_consumed_and_signing_rollback_is_refused_after_expiry() {
    let mut authority = fixture();
    let session = authority
        .initialize("init", "init", "administrator", "unique-password-example")
        .unwrap();
    authority.require_admin(&session).unwrap();
    assert!(
        authority
            .initialize("init", "init", "other", "unique-password-example")
            .is_err()
    );
    let mut higher = authority.signing.generation;
    higher.number = 2;
    save(&mut authority.store, "signing-generation", &higher).unwrap();
    assert!(
        authority
            .signing
            .activate(&mut authority.store, authority.environment.now())
            .is_err()
    );
}

#[test]
fn global_creation_rate_limit_preserves_innocent_source_state_and_reports_remaining_wait() {
    let mut authority = fixture();
    let settings = Settings {
        registration: RegistrationPolicy::Automatic,
        source_rate: Rate {
            burst: 1,
            refill_seconds: 600,
        },
        global_rate: Rate {
            burst: 1,
            refill_seconds: 2,
        },
        ..Settings::default()
    };
    save(&mut authority.store, "settings", &settings).unwrap();

    authority.register("noisy", registration(1)).unwrap();
    authority.environment.now += 1;
    let writes_before = authority.store.put_count;
    for attempt in 0..2 {
        let fault = authority
            .register("innocent", registration(2))
            .unwrap_err()
            .downcast::<Fault>()
            .unwrap();
        assert!(
            matches!(fault, Fault::RateLimited { retry_after: 1 }),
            "attempt {attempt} returned {fault:?}"
        );
    }
    assert_eq!(authority.store.put_count, writes_before);

    authority.environment.now += 1;
    // Prior global-only refusals must not escalate an innocent source to PoW.
    authority.register("innocent", registration(2)).unwrap();

    authority.environment.now += 98;
    let fault = authority
        .register("noisy", registration(3))
        .unwrap_err()
        .downcast::<Fault>()
        .unwrap();
    assert!(matches!(fault, Fault::RateLimited { retry_after: 500 }));
}

#[test]
fn usage_report_rejects_invalid_monthly_relay_budget() {
    let mut authority = fixture();
    let node = SigningKey::from_bytes(&[9; 32]);
    let node_key = node.verifying_key().to_bytes();
    save(
        &mut authority.store,
        &key("node", &node_key),
        &Node {
            listing: None,
            version: 1,
            key: node_key,
            zone: Zone::Global,
            region: "test".into(),
            anonymous: true,
            enabled: true,
        },
    )
    .unwrap();
    for budget in [
        windshare_relay_access::usage::MonthlyBudget {
            bytes: 1024,
            reset_day_utc: 0,
        },
        windshare_relay_access::usage::MonthlyBudget {
            bytes: windshare_relay_access::usage::MonthlyBudget::MAX_BYTES + 1,
            reset_day_utc: 15,
        },
    ] {
        let invalid = Batch {
            id: [5; 16],
            node: node_key,
            issuer: authority.signing.generation.issuer,
            created_at: authority.environment.now(),
            records: vec![],
            resident_accounts: vec![],
            shares: vec![],
            load: Load {
                relay_budget: Some(budget),
                active_sessions: 0,
                wire_bytes: 0,
                forwarded_bytes: 0,
                admission_refusals: 0,
            },
        };
        let signed = trust::sign(USAGE_DOMAIN, &serde_json::to_vec(&invalid).unwrap(), &node);
        let fault = authority
            .usage(&signed)
            .unwrap_err()
            .downcast::<Fault>()
            .unwrap();
        assert!(matches!(fault, Fault::Invalid));
    }
}
