use super::*;
use sha2::{Digest, Sha256};

const SOURCE_REFILL_SECONDS: u64 = 240;
const GLOBAL_REFILL_SECONDS: u64 = 120;

fn work_authority(source_burst: u64, global_burst: u64) -> Authority<Memory, Clock> {
    let mut authority = fixture();
    let settings = Settings {
        registration: RegistrationPolicy::ProofOfWork,
        pow_bits: 4,
        source_rate: Rate {
            burst: source_burst,
            refill_seconds: SOURCE_REFILL_SECONDS,
        },
        global_rate: Rate {
            burst: global_burst,
            refill_seconds: GLOBAL_REFILL_SECONDS,
        },
        ..Settings::default()
    };
    save(&mut authority.store, "settings", &settings).unwrap();
    authority
}

fn registration_fault(
    authority: &mut Authority<Memory, Clock>,
    source: &str,
    request: Registration,
) -> Fault {
    authority
        .register(source, request)
        .unwrap_err()
        .downcast::<Fault>()
        .unwrap()
}

fn work_challenge(authority: &mut Authority<Memory, Clock>, source: &str, secret: u8) -> Challenge {
    match registration_fault(authority, source, registration(secret)) {
        Fault::Verification { challenge } if challenge.mode == RegistrationMode::ProofOfWork => {
            challenge
        }
        other => panic!("expected a work challenge, got {other:?}"),
    }
}

fn solved(secret: u8, challenge: Challenge) -> Registration {
    let solution = (0..4096)
        .map(|nonce| nonce.to_string())
        .find(|nonce| {
            let hash = Sha256::new()
                .chain_update(challenge.id.as_bytes())
                .chain_update(nonce.as_bytes())
                .finalize();
            hash[0].leading_zeros() >= u32::from(challenge.bits)
        })
        .unwrap();
    Registration {
        challenge: Some(challenge.id),
        solution: Some(solution),
        ..registration(secret)
    }
}

#[test]
fn shared_source_registrations_keep_their_work_and_share_creation_credit() {
    let mut authority = work_authority(2, 10);
    let first = work_challenge(&mut authority, "shared", 1);
    let second = work_challenge(&mut authority, "shared", 2);
    let third = work_challenge(&mut authority, "shared", 3);
    assert_ne!(first.id, second.id);
    assert_eq!(first.id, work_challenge(&mut authority, "shared", 1).id);

    let first = authority.register("shared", solved(1, first)).unwrap();
    let second = authority.register("shared", solved(2, second)).unwrap();
    assert_ne!(first.account, second.account);
    assert!(matches!(
        registration_fault(&mut authority, "shared", solved(3, third)),
        Fault::RateLimited { .. }
    ));
    assert_eq!(authority.account_count().unwrap(), 2);
}

#[test]
fn issuance_budgets_only_charge_new_challenges() {
    let mut authority = work_authority(1, 2);
    let first = work_challenge(&mut authority, "shared", 1);
    for secret in 2..=4 {
        work_challenge(&mut authority, "shared", secret);
    }
    assert!(matches!(
        registration_fault(&mut authority, "shared", registration(5)),
        Fault::RateLimited { retry_after: 60 }
    ));
    for secret in 5..=8 {
        work_challenge(&mut authority, "other", secret);
    }
    let writes = authority.store.put_count;
    assert!(matches!(
        registration_fault(&mut authority, "third", registration(9)),
        Fault::RateLimited { retry_after: 30 }
    ));
    assert_eq!(authority.store.put_count, writes);
    assert_eq!(first.id, work_challenge(&mut authority, "shared", 1).id);
    assert_eq!(authority.store.put_count, writes);
    authority.register("shared", solved(1, first)).unwrap();

    authority.environment.now += 30;
    work_challenge(&mut authority, "third", 9);
}

#[test]
fn issuing_work_reclaims_expired_challenges_without_evicting_live_attempts() {
    let mut authority = work_authority(3, 10);
    let expired = work_challenge(&mut authority, "shared", 1);
    authority.environment.now += 1;
    let live = work_challenge(&mut authority, "shared", 2);
    authority.environment.now = expired.expires_at;
    work_challenge(&mut authority, "shared", 3);

    let prefix = format!("{}/", ProofKind::Work.registration_prefix());
    assert_eq!(authority.store.scan(&prefix, None).unwrap().len(), 2);
    assert_eq!(live.id, work_challenge(&mut authority, "shared", 2).id);
}
