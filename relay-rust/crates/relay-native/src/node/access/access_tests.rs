use super::*;
use crate::connection::AdmissionPriority;
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ed25519_dalek::SigningKey;
use windshare_relay_access::{
    Credential, CredentialState, Digest,
    entitlement::{DAY_SECONDS, ENTITLEMENT_LIFETIME},
    ticket::Ticket,
    trust::Generation,
};
use windshare_relay_core::access::RegistrationDecision;

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
    let token = Bearer::new(&URL_SAFE_NO_PAD.encode(ticket.issue(generation, &key).unwrap()));
    let Authorization::Granted {
        attribution,
        class,
        allowance,
    } = service.authorize(share(1), Some(&token), NOW)
    else {
        panic!("valid ticket refused");
    };
    assert_eq!(
        (class, allowance),
        (Class::Standard, Some(Allowance::Available))
    );
    let first = service.reserve(share(1), attribution).unwrap();
    let resumed = service.reserve(share(1), attribution).unwrap();
    let sibling = Ticket {
        credential: [5; 16],
        ..ticket
    }
    .attribution();
    assert!(matches!(
        service.reserve(share(2), sibling),
        Err(Refusal {
            decision: RegistrationDecision::Unavailable(_),
            reason: RefusalReason::PrincipalShareLimit,
        })
    ));
    drop(first);
    assert!(service.reserve(share(2), sibling).is_err());
    drop(resumed);
    assert!(service.reserve(share(2), sibling).is_ok());
}

#[test]
fn old_tickets_do_not_revive_allowance_or_revoked_credentials() {
    let (service, ticket, key, generation, _) = identity();
    let mut token = Bearer::new(&URL_SAFE_NO_PAD.encode(ticket.issue(generation, &key).unwrap()));
    let attribution = ticket.attribution();
    assert_eq!(
        service.admission_priority(share(1), Some(&mut token), NOW),
        AdmissionPriority::Authenticated
    );
    assert_eq!(service.class(attribution, NOW).1, Some(Allowance::Stale));
    assert!(matches!(
        service.authorize(share(1), Some(&token), NOW),
        Authorization::Granted {
            class: Class::Standard,
            allowance: Some(Allowance::Available),
            ..
        }
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
        Authorization::Granted {
            class: Class::Lowest,
            allowance: Some(Allowance::Spent),
            ..
        }
    ));
    assert_eq!(
        service.class(attribution, NOW),
        (Class::Lowest, Some(Allowance::Spent))
    );
    service.expire_quotas(spent.valid_until);
    assert!(matches!(
        service.authorize(share(1), Some(&token), spent.valid_until),
        Authorization::Granted {
            class: Class::Lowest,
            allowance: Some(Allowance::Stale),
            ..
        }
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
        Authorization::Refused(Refusal {
            decision: RegistrationDecision::Denied,
            reason: RefusalReason::CredentialRestricted,
        })
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
fn registration_retries_generation_mismatches_and_revalidates_cached_proof() {
    let (service, ticket, key, generation, _) = identity();
    let previous_token = URL_SAFE_NO_PAD.encode(ticket.issue(generation, &key).unwrap());
    let mut previous_bearer = Bearer::new(&previous_token);
    assert_eq!(
        service.admission_priority(share(1), Some(&mut previous_bearer), NOW),
        AdmissionPriority::Authenticated
    );
    assert!(matches!(
        service.authorize(share(1), Some(&previous_bearer), ticket.expires_at),
        Authorization::Refused(Refusal {
            decision: RegistrationDecision::Denied,
            reason: RefusalReason::TicketExpired,
        })
    ));

    let next_key = SigningKey::from_bytes(&[8; 32]);
    let next_generation = Generation {
        number: generation.number + 1,
        key: next_key.verifying_key().to_bytes(),
        ..generation
    };
    let next_ticket = Ticket {
        key_id: next_generation.number,
        ..ticket
    };
    let next_token = URL_SAFE_NO_PAD.encode(next_ticket.issue(next_generation, &next_key).unwrap());
    let mut next_bearer = Bearer::new(&next_token);
    assert_eq!(
        service.admission_priority(share(1), Some(&mut next_bearer), NOW),
        AdmissionPriority::Anonymous
    );
    assert!(matches!(
        service.authorize(share(1), Some(&next_bearer), NOW),
        Authorization::Refused(Refusal {
            decision: RegistrationDecision::Unavailable(_),
            reason: RefusalReason::TicketGenerationMismatch,
        })
    ));
    assert!(matches!(
        service.authorize(share(1), Some(&next_bearer), next_ticket.expires_at),
        Authorization::Refused(Refusal {
            decision: RegistrationDecision::Denied,
            reason: RefusalReason::TicketExpired,
        })
    ));

    let mut restrictions = Restrictions {
        version: 1,
        ..Restrictions::default()
    };
    restrictions.shares.insert(*share(2).as_bytes());
    assert!(service.restrict(ticket.issuer, restrictions));
    assert!(matches!(
        service.authorize(share(2), Some(&next_bearer), NOW),
        Authorization::Refused(Refusal {
            decision: RegistrationDecision::Denied,
            reason: RefusalReason::ShareRestricted,
        })
    ));

    service.apply_authority(next_generation.into()).unwrap();
    for stale_bearer in [previous_bearer, Bearer::new(&previous_token)] {
        assert!(matches!(
            service.authorize(share(1), Some(&stale_bearer), NOW + 1),
            Authorization::Refused(Refusal {
                decision: RegistrationDecision::Unavailable(_),
                reason: RefusalReason::TicketGenerationMismatch,
            })
        ));
    }
    assert!(matches!(
        service.authorize(share(1), Some(&next_bearer), NOW + 1),
        Authorization::Granted {
            attribution,
            class: Class::Standard,
            allowance: Some(Allowance::Available),
        } if attribution == ticket.attribution()
    ));

    service.set_controller(Some(ticket.issuer));
    service
        .apply_authority(AuthorityState {
            generation: next_generation,
            policy: Some(windshare_relay_access::usage::NodePolicy {
                version: 1,
                enabled: false,
                anonymous: false,
            }),
        })
        .unwrap();
    assert!(matches!(
        service.authorize(share(1), Some(&Bearer::new(&previous_token)), NOW + 1),
        Authorization::Refused(Refusal {
            decision: RegistrationDecision::Denied,
            reason: RefusalReason::NodePublicDisabled,
        })
    ));
}

#[test]
fn removing_an_issuer_disables_new_admission_and_preserves_generation_high_water() {
    let (service, ticket, key, generation, mut policy) = identity();
    let mut token = Bearer::new(&URL_SAFE_NO_PAD.encode(ticket.issue(generation, &key).unwrap()));
    assert_eq!(
        service.admission_priority(share(1), Some(&mut token), NOW),
        AdmissionPriority::Authenticated
    );
    service
        .apply_authority(
            Generation {
                number: generation.number + 1,
                ..generation
            }
            .into(),
        )
        .unwrap();
    policy.trusted_issuers.clear();
    service.configure(policy).unwrap();
    assert!(matches!(
        service.authorize(share(1), Some(&token), NOW),
        Authorization::Refused(Refusal {
            decision: RegistrationDecision::Denied,
            reason: RefusalReason::IssuerInactive,
        })
    ));
    assert!(matches!(
        service.authorize(share(1), None, NOW),
        Authorization::Refused(Refusal {
            decision: RegistrationDecision::Denied,
            reason: RefusalReason::AnonymousDisabled,
        })
    ));
    assert!(matches!(
        service.authorize(share(1), Some(&Bearer::new("invalid")), NOW),
        Authorization::Refused(Refusal {
            decision: RegistrationDecision::Denied,
            reason: RefusalReason::TicketEncoding,
        })
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
        Err(Refusal {
            decision: RegistrationDecision::Unavailable(_),
            reason: RefusalReason::ResidentShareCapacity,
        })
    ));
    let mut token = Bearer::new("owner-secret");
    assert_eq!(
        service.admission_priority(share(2), Some(&mut token), NOW),
        AdmissionPriority::Owner
    );
    assert_eq!(
        service.credential_by_id([2; 16]).unwrap().last_used_at,
        None
    );
    let Authorization::Granted {
        attribution: owner,
        class: Class::Owner,
        allowance: None,
    } = service.authorize(share(2), Some(&token), NOW)
    else {
        panic!("owner refused");
    };
    let _owner = service.reserve(share(2), owner).unwrap();
    assert_eq!(
        service.credential_by_id([2; 16]).unwrap().last_used_at,
        Some(NOW * 1000)
    );
    assert_eq!(service.class(owner, NOW).0, Class::Owner);
    drop(public);
    assert!(service.allowed(share(2), owner));
}

#[test]
fn owner_can_use_all_reserved_shares_beyond_per_principal_limit() {
    let service = Service::default();
    let owner_credential = Credential {
        principal: [1; 16],
        id: [2; 16],
        name: "owner".into(),
        digest: Digest::of(b"owner-secret"),
        created_at: NOW,
        last_used_at: None,
        state: CredentialState::Active,
    };
    let guest_credential = Credential {
        principal: [3; 16],
        id: [4; 16],
        name: "guest".into(),
        digest: Digest::of(b"guest-secret"),
        created_at: NOW,
        last_used_at: None,
        state: CredentialState::Active,
    };
    let default_policy = Policy::default();
    let owner_reserve = default_policy.owner_reserved_shares;
    let per_principal = default_policy.shares_per_principal;
    assert!(owner_reserve > per_principal);
    service
        .configure(Policy {
            owner_principals: vec![owner_credential.principal],
            credentials: vec![owner_credential, guest_credential],
            ..default_policy
        })
        .unwrap();

    let owner = Attribution::Credential {
        principal: [1; 16],
        credential: [2; 16],
    };
    let guest = Attribution::Credential {
        principal: [3; 16],
        credential: [4; 16],
    };
    let mut owner_shares = Vec::with_capacity(owner_reserve);
    for index in 1..=u8::try_from(owner_reserve).unwrap() {
        owner_shares.push(service.reserve(share(index), owner).unwrap());
    }
    let mut guest_shares = Vec::with_capacity(per_principal);
    for index in 1..=u8::try_from(per_principal).unwrap() {
        guest_shares.push(service.reserve(share(100 + index), guest).unwrap());
    }
    assert!(matches!(
        service.reserve(share(200), guest),
        Err(Refusal {
            decision: RegistrationDecision::Unavailable(_),
            reason: RefusalReason::PrincipalShareLimit,
        })
    ));
    drop(guest_shares);
    drop(owner_shares);
}

#[test]
fn with_forwarding_allows_concurrent_readers_and_fences_revocation() {
    use std::sync::{
        Barrier,
        atomic::{AtomicBool, Ordering},
        mpsc,
    };

    let (service, ticket, _, _, _) = identity();
    let attribution = ticket.attribution();

    // Two share actors must enter `with_forwarding` in parallel; an exclusive lock would deadlock.
    let barrier = Arc::new(Barrier::new(2));
    let other_service = service.clone();
    let other_barrier = Arc::clone(&barrier);
    let peer = std::thread::spawn(move || {
        other_service
            .with_forwarding(share(2), attribution, || {
                other_barrier.wait();
                true
            })
            .unwrap()
    });
    assert!(
        service
            .with_forwarding(share(1), attribution, || {
                barrier.wait();
                true
            })
            .unwrap()
    );
    assert!(peer.join().unwrap());

    // A restriction write must wait for any in-flight forwarding closure to finish.
    let (entered_tx, entered_rx) = mpsc::channel();
    let (release_tx, release_rx) = mpsc::channel();
    let (revoker_started_tx, revoker_started_rx) = mpsc::channel();
    let restricted_finished = Arc::new(AtomicBool::new(false));

    let forward_service = service.clone();
    let forwarder = std::thread::spawn(move || {
        forward_service.with_forwarding(share(1), attribution, || {
            entered_tx.send(()).unwrap();
            release_rx.recv().unwrap();
            42
        })
    });
    entered_rx.recv().unwrap();

    let revoke_service = service.clone();
    let revoke_done = Arc::clone(&restricted_finished);
    let revoker = std::thread::spawn(move || {
        let mut restrictions = Restrictions {
            version: 1,
            ..Restrictions::default()
        };
        restrictions.shares.insert(*share(1).as_bytes());
        revoker_started_tx.send(()).unwrap();
        assert!(revoke_service.restrict(ticket.issuer, restrictions));
        revoke_done.store(true, Ordering::Release);
    });
    revoker_started_rx.recv().unwrap();

    // Release the in-flight forwarder and ensure revocation then rejects subsequent forwards.
    assert!(!restricted_finished.load(Ordering::Acquire));
    release_tx.send(()).unwrap();
    assert_eq!(forwarder.join().unwrap(), Some(42));
    revoker.join().unwrap();
    assert!(restricted_finished.load(Ordering::Acquire));
    assert!(
        service
            .with_forwarding(share(1), attribution, || unreachable!())
            .is_none()
    );
}

#[test]
fn expire_quotas_skips_write_lock_until_a_deadline_is_due() {
    let (service, ticket, _, _, _) = identity();
    let attribution = ticket.attribution();
    assert!(service.refresh(ticket.issuer, ticket.account, ticket.entitlement, NOW));
    // Establish the current day floor once; steady-state ticks within the same day need no writer.
    service.expire_quotas(NOW);
    assert_eq!(
        service.class(attribution, NOW).1,
        Some(Allowance::Available)
    );

    // Holding a shared read lock while another thread runs steady-state `expire_quotas`
    // proves it takes only a shared read lock and never blocks waiting for a writer.
    let guard = service.read();
    let peer = service.clone();
    std::thread::spawn(move || peer.expire_quotas(NOW + 1))
        .join()
        .unwrap();
    drop(guard);

    service.expire_quotas(ticket.entitlement.valid_until);
    assert_eq!(
        service.class(attribution, ticket.entitlement.valid_until).1,
        Some(Allowance::Stale)
    );
}
