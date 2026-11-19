use super::*;
#[path = "contract_vectors_tests.rs"]
mod contract_vectors;
#[path = "device_login_tests.rs"]
mod device_login;
#[path = "governance_tests.rs"]
mod governance;
#[path = "profile_tests.rs"]
mod profile;
use crate::store::{key, load, save};
use anyhow::Result;
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ed25519_dalek::SigningKey;
use std::collections::BTreeMap;
use windshare_relay_access::{
    entitlement::{DAY_SECONDS, ENTITLEMENT_LIFETIME},
    ticket::Ticket,
    trust::{self, Generation, TrustStore},
    usage::{Batch, Load, RESPONSE_DOMAIN, Reply, USAGE_DOMAIN, UsageRecord},
};

#[derive(Clone, Default)]
struct Memory(BTreeMap<String, String>);
impl Store for Memory {
    fn get(&self, key: &str) -> Result<Option<String>> {
        Ok(self.0.get(key).cloned())
    }
    fn put(&mut self, key: &str, value: &str) -> Result<()> {
        self.0.insert(key.into(), value.into());
        Ok(())
    }
    fn delete(&mut self, key: &str) -> Result<()> {
        self.0.remove(key);
        Ok(())
    }
    fn scan(&self, prefix: &str, limit: Option<usize>) -> Result<Vec<(String, String)>> {
        Ok(self
            .0
            .iter()
            .filter(|(key, _)| key.starts_with(prefix))
            .take(limit.unwrap_or(usize::MAX))
            .map(|(key, value)| (key.clone(), value.clone()))
            .collect())
    }
}
struct Clock {
    now: u64,
    next: u8,
}
impl Environment for Clock {
    fn now(&self) -> u64 {
        self.now
    }
    fn random<const N: usize>(&mut self) -> Result<[u8; N]> {
        self.next += 1;
        Ok([self.next; N])
    }
}
fn fixture() -> Authority<Memory, Clock> {
    let root = SigningKey::from_bytes(&[41; 32]);
    let signing = SigningKey::from_bytes(&[42; 32]);
    let generation = Generation {
        issuer: root.verifying_key().to_bytes(),
        number: 1,
        key: signing.verifying_key().to_bytes(),
        not_before: DAY_SECONDS,
        expires_at: DAY_SECONDS * 30,
    };
    let certificate = URL_SAFE_NO_PAD.encode(generation.certify(&root).unwrap());
    Authority::new(
        Memory::default(),
        Clock {
            now: DAY_SECONDS * 2,
            next: 10,
        },
        Signing {
            generation,
            key: signing,
            certificate,
        },
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
        Ok((delivery.accepted(&mut self.store)?, code))
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
        resident_accounts: vec![account],
        shares: vec![],
        load: Load {
            active_sessions: 1,
            wire_bytes: bytes,
            forwarded_bytes: bytes,
            admission_refusals: 0,
        },
    };
    trust::sign(USAGE_DOMAIN, &serde_json::to_vec(&batch).unwrap(), node)
}
fn reply(authority: &mut Authority<Memory, Clock>, bytes: &[u8]) -> Reply {
    let response = authority.usage(bytes).unwrap();
    let body = URL_SAFE_NO_PAD.decode(response.body).unwrap();
    serde_json::from_slice(
        trust::verify(RESPONSE_DOMAIN, &body, &authority.signing.generation.key).unwrap(),
    )
    .unwrap()
}
#[test]
fn setup_retry_reuses_principal_and_refresh_reuses_credential() {
    let mut authority = fixture();
    let first = authority.register("source", registration(1)).unwrap();
    let retry = authority.register("source", registration(1)).unwrap();
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
    assert!(retry.entitlement.version > first.entitlement.version);
    assert_eq!(retry.entitlement.valid_until, first.entitlement.valid_until);
}
#[test]
fn late_reports_charge_the_original_day_and_restart_retries_do_not_double_charge() {
    let mut authority = fixture();
    let session = authority.register("source", registration(1)).unwrap();
    let node = SigningKey::from_bytes(&[3; 32]);
    save(
        &mut authority.store,
        &key("node", &node.verifying_key().to_bytes()),
        &Node {
            version: 1,
            key: node.verifying_key().to_bytes(),
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
    let first = reply(&mut authority, &report);
    assert_eq!(first.entitlements[0].entitlement.remaining_bytes, 1000);
    let store = authority.store.clone();
    authority = Authority::new(store, authority.environment, authority.signing).unwrap();
    let retry = reply(&mut authority, &report);
    assert_eq!(
        load::<u64>(
            &authority.store,
            &format!("{}/1", key("usage", &session.account))
        )
        .unwrap(),
        Some(700)
    );
    assert!(retry.entitlements[0].entitlement.version > first.entitlements[0].entitlement.version);
    let current = batch(&authority, &node, session.account, 2, 2, 300);
    assert_eq!(
        reply(&mut authority, &current).entitlements[0]
            .entitlement
            .remaining_bytes,
        700
    );
    authority.environment.now += ENTITLEMENT_LIFETIME - 1;
    let refresh = batch(&authority, &node, session.account, 3, 2, 0);
    assert!(
        reply(&mut authority, &refresh).entitlements[0]
            .entitlement
            .valid_until
            > retry.entitlements[0].entitlement.valid_until
    );
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
            .restrictions()
            .unwrap()
            .credentials
            .contains(&second.id)
    );
}
#[test]
fn creation_bursts_do_not_limit_existing_accounts_and_milestones_do_not_change_modes() {
    let mut authority = fixture();
    let settings = Settings {
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
        RegistrationMode::Automatic
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
