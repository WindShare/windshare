use super::*;
use ed25519_dalek::SigningKey;
use windshare_relay_access::Credential;
use windshare_relay_access::{
    entitlement::{DAY_SECONDS, ENTITLEMENT_LIFETIME},
    trust::Generation,
};

#[test]
fn newer_policy_revokes_forwarding_and_older_reports_cannot_restore_it() {
    let (service, ticket, _, generation, _) = identity();
    service.set_controller(Some(ticket.issuer));
    let policy = |version, enabled| AuthorityState {
        generation,
        policy: Some(windshare_relay_access::usage::NodePolicy {
            version,
            enabled,
            anonymous: true,
        }),
    };
    service.apply_authority(policy(1, true)).unwrap();
    assert!(service.allowed(share(1), ticket.attribution()));
    assert!(service.anonymous());
    service.apply_authority(policy(2, false)).unwrap();
    service.apply_authority(policy(1, true)).unwrap();
    assert!(!service.public_enabled());
    assert!(!service.anonymous());
    assert!(!service.allowed(share(1), ticket.attribution()));
    assert!(!service.allowed(share(1), Attribution::Anonymous));
    service.apply_authority(policy(3, true)).unwrap();
    assert!(service.allowed(share(1), ticket.attribution()));
}

const NOW: u64 = 20 * DAY_SECONDS + 120;

fn identity() -> (Service, Ticket, SigningKey, Generation, Policy) {
    let root = SigningKey::from_bytes(&[1; 32]);
    let key = SigningKey::from_bytes(&[2; 32]);
    let generation = Generation {
        issuer: root.verifying_key().to_bytes(),
        number: 1,
        key: key.verifying_key().to_bytes(),
        not_before: NOW - 100,
        expires_at: NOW + DAY_SECONDS,
    };
    let ticket = Ticket {
        issuer: generation.issuer,
        key_id: 1,
        account: [3; 16],
        credential: [4; 16],
        issued_at: NOW,
        expires_at: NOW + 3600,
        entitlement: Entitlement {
            version: 1,
            day: NOW / DAY_SECONDS,
            valid_until: NOW + ENTITLEMENT_LIFETIME,
            remaining_bytes: 1000,
        },
    };
    let policy = Policy {
        anonymous: false,
        shares_per_principal: 1,
        trusted_issuers: vec![TrustedIssuer {
            root: URL_SAFE_NO_PAD.encode(generation.issuer),
            certificate: URL_SAFE_NO_PAD.encode(generation.certify(&root).unwrap()),
        }],
        ..Policy::default()
    };
    let service = Service::default();
    service.configure(policy.clone()).unwrap();
    (service, ticket, key, generation, policy)
}

fn share(value: u8) -> ShareId {
    ShareId::new([value; 12]).unwrap()
}

#[test]
fn offline_tickets_share_a_resident_limit_and_reconnect_reuses_it() {
    let (service, ticket, key, generation, _) = identity();
    let token = URL_SAFE_NO_PAD.encode(ticket.issue(generation, &key).unwrap());
    let RegistrationDecision::Granted(attribution) = service.authorize(share(1), Some(&token), NOW)
    else {
        panic!("valid ticket refused");
    };
    let first = service.reserve(share(1), attribution).unwrap();
    let resumed = service.reserve(share(1), attribution).unwrap();
    let sibling = Ticket {
        credential: [5; 16],
        ..ticket
    }
    .attribution();
    assert!(matches!(
        service.reserve(share(2), sibling),
        Err(RegistrationDecision::Unavailable)
    ));
    drop(first);
    assert!(service.reserve(share(2), sibling).is_err());
    drop(resumed);
    assert!(service.reserve(share(2), sibling).is_ok());
}

#[test]
fn old_tickets_do_not_revive_allowance_or_revoked_credentials() {
    let (service, ticket, key, generation, _) = identity();
    let token = URL_SAFE_NO_PAD.encode(ticket.issue(generation, &key).unwrap());
    let attribution = ticket.attribution();
    assert!(matches!(
        service.authorize(share(1), Some(&token), NOW),
        RegistrationDecision::Granted(_)
    ));
    let spent = Entitlement {
        version: 2,
        valid_until: NOW + 1,
        remaining_bytes: 0,
        ..ticket.entitlement
    };
    assert!(service.refresh(ticket.issuer, ticket.account, spent, NOW));
    assert!(matches!(
        service.authorize(share(1), Some(&token), NOW),
        RegistrationDecision::Granted(_)
    ));
    assert_eq!(
        service.class(attribution, NOW),
        (Class::Lowest, Some(Allowance::Spent))
    );
    service.expire_quotas(spent.valid_until);
    assert!(matches!(
        service.authorize(share(1), Some(&token), spent.valid_until),
        RegistrationDecision::Granted(_)
    ));
    assert_eq!(
        service.class(attribution, spent.valid_until),
        (Class::Lowest, Some(Allowance::Stale))
    );
    let mut restrictions = Restrictions {
        version: 2,
        ..Restrictions::default()
    };
    restrictions.credentials.insert(ticket.credential);
    assert!(service.restrict(ticket.issuer, restrictions));
    assert!(!service.allowed(share(1), attribution));
    assert!(matches!(
        service.authorize(share(1), Some(&token), NOW),
        RegistrationDecision::Denied
    ));
    assert!(service.refresh(
        ticket.issuer,
        ticket.account,
        Entitlement {
            version: 3,
            ..ticket.entitlement
        },
        NOW,
    ));
    assert!(!service.allowed(share(1), attribution));
    assert!(!service.restrict(
        ticket.issuer,
        Restrictions {
            version: 1,
            ..Restrictions::default()
        }
    ));
}

#[test]
fn removing_an_issuer_disables_new_admission_and_preserves_generation_high_water() {
    let (service, ticket, key, generation, mut policy) = identity();
    let token = URL_SAFE_NO_PAD.encode(ticket.issue(generation, &key).unwrap());
    policy.trusted_issuers.clear();
    service.configure(policy).unwrap();
    assert!(matches!(
        service.authorize(share(1), Some(&token), NOW),
        RegistrationDecision::Denied
    ));
    assert!(matches!(
        service.authorize(share(1), None, NOW),
        RegistrationDecision::Denied
    ));
    assert!(matches!(
        service.authorize(share(1), Some("invalid"), NOW),
        RegistrationDecision::Denied
    ));
}

#[test]
fn local_owner_capacity_is_preserved_when_public_residency_is_full() {
    let service = Service::default();
    let credential = Credential {
        principal: [1; 16],
        id: [2; 16],
        name: "owner".into(),
        digest: Digest::of(b"owner-secret"),
        created_at: NOW,
        last_used_at: None,
        state: CredentialState::Active,
    };
    service
        .configure(Policy {
            owner_reserved_shares: crate::config::MAX_RESIDENT_SHARES - 1,
            owner_principals: vec![credential.principal],
            credentials: vec![credential],
            ..Policy::default()
        })
        .unwrap();
    let public = service.reserve(share(1), Attribution::Anonymous).unwrap();
    assert!(matches!(
        service.reserve(share(2), Attribution::Anonymous),
        Err(RegistrationDecision::Unavailable)
    ));
    let RegistrationDecision::Granted(owner) =
        service.authorize(share(2), Some("owner-secret"), NOW)
    else {
        panic!("owner refused");
    };
    let _owner = service.reserve(share(2), owner).unwrap();
    assert_eq!(service.class(owner, NOW).0, Class::Owner);
    drop(public);
    assert!(service.allowed(share(2), owner));
}
