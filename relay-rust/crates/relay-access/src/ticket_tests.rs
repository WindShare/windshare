use super::*;
use crate::entitlement::{Allowance, Class, DAY_SECONDS, ENTITLEMENT_LIFETIME, advance};
use crate::trust::{Error, MAX_CERTIFICATE_LIFETIME};

const NOW: u64 = 20 * DAY_SECONDS + 120;

fn fixture() -> (SigningKey, SigningKey, Generation, Ticket, TrustStore) {
    let root = SigningKey::from_bytes(&[11; 32]);
    let online = SigningKey::from_bytes(&[22; 32]);
    let generation = Generation {
        issuer: root.verifying_key().to_bytes(),
        number: 1,
        key: online.verifying_key().to_bytes(),
        not_before: NOW - 120,
        expires_at: NOW + DAY_SECONDS,
    };
    let ticket = Ticket {
        issuer: generation.issuer,
        key_id: 1,
        account: [1; 16],
        credential: [2; 16],
        issued_at: NOW,
        expires_at: NOW + TICKET_LIFETIME,
        entitlement: Entitlement {
            version: 1,
            day: NOW / DAY_SECONDS,
            valid_until: NOW + ENTITLEMENT_LIFETIME,
            remaining_bytes: 100,
        },
    };
    let certified =
        Generation::verify(&generation.certify(&root).unwrap(), generation.issuer).unwrap();
    let mut trust = TrustStore::default();
    trust.accept(certified, NOW).unwrap();
    (root, online, generation, ticket, trust)
}

#[test]
fn identity_outlives_entitlement_without_replenishing_it() {
    let (_, key, generation, ticket, trust) = fixture();
    let signed = ticket.issue(generation, &key).unwrap();
    let verified = Ticket::verify(&signed, &trust, NOW + ENTITLEMENT_LIFETIME).unwrap();
    assert_eq!(verified.attribution(), ticket.attribution());
    assert_eq!(
        verified.entitlement.allowance(NOW + ENTITLEMENT_LIFETIME),
        Allowance::Stale
    );
    assert_eq!(Entitlement::class(Allowance::Stale), Class::Lowest);
    let spent = Entitlement {
        version: 2,
        remaining_bytes: 0,
        ..ticket.entitlement
    };
    let mut cached = Some(spent);
    assert!(!advance(&mut cached, verified.entitlement));
    assert_eq!(cached, Some(spent));
    assert_eq!(spent.allowance(NOW), Allowance::Spent);
    assert_eq!(
        spent.allowance((spent.day + 1) * DAY_SECONDS),
        Allowance::Stale
    );
}

#[test]
fn signed_identity_rejects_corruption_wrong_issuer_expiry_and_wrong_audience() {
    let (_, key, generation, ticket, trust) = fixture();
    let mut signed = ticket.issue(generation, &key).unwrap();
    signed[20] ^= 1;
    assert!(Ticket::verify(&signed, &trust, NOW).is_err());
    let signed = ticket.issue(generation, &key).unwrap();
    assert_eq!(
        Ticket::verify(&signed, &TrustStore::default(), NOW),
        Err(Error::Untrusted)
    );
    assert_eq!(
        Ticket::verify(&signed, &trust, ticket.expires_at),
        Err(Error::Expired)
    );
    let mut payload = ticket.encode();
    let offset = payload
        .windows(AUDIENCE.len())
        .position(|bytes| bytes == AUDIENCE.as_bytes())
        .unwrap();
    payload[offset] = b'x';
    assert_eq!(
        Ticket::verify(&trust::sign(TICKET_DOMAIN, &payload, &key), &trust, NOW),
        Err(Error::Encoding)
    );
}

#[test]
fn certified_generations_reject_rollback_equivocation_and_excess_lifetime() {
    let (root, key, generation, ticket, mut trust) = fixture();
    let second = Generation {
        number: 2,
        ..generation
    };
    trust
        .accept(
            Generation::verify(&second.certify(&root).unwrap(), second.issuer).unwrap(),
            NOW,
        )
        .unwrap();
    assert_eq!(trust.accept(generation, NOW), Err(Error::Rollback));
    assert_eq!(
        trust.accept(
            Generation {
                key: [3; 32],
                ..second
            },
            NOW
        ),
        Err(Error::Rollback)
    );
    assert_eq!(
        Ticket::verify(&ticket.issue(generation, &key).unwrap(), &trust, NOW),
        Err(Error::Rollback)
    );
    assert_eq!(
        Generation {
            expires_at: generation.not_before + MAX_CERTIFICATE_LIFETIME + 1,
            ..generation
        }
        .certify(&root),
        Err(Error::Lifetime)
    );
    let certificate = generation.certify(&root).unwrap();
    assert_eq!(
        Generation::verify(&certificate, key.verifying_key().to_bytes()),
        Err(Error::Signature)
    );
}

#[test]
fn attribution_separates_issuer_credential_and_principal() {
    let (_, _, _, ticket, _) = fixture();
    let first = ticket.attribution();
    let second = Ticket {
        credential: [3; 16],
        ..ticket
    }
    .attribution();
    let outsider = Ticket {
        issuer: [4; 32],
        ..ticket
    }
    .attribution();
    assert_eq!(first.principal([0; 12]), second.principal([0; 12]));
    assert_ne!(first.credential(), second.credential());
    assert_ne!(first.principal([0; 12]), outsider.principal([0; 12]));
    for attribution in [
        first,
        Attribution::Anonymous,
        Attribution::Credential {
            principal: [6; 16],
            credential: [7; 16],
        },
    ] {
        assert_eq!(Attribution::decode(attribution.encode()), Some(attribution));
    }
}

#[test]
fn ticket_rejects_noncanonical_encoding_and_unbounded_entitlements() {
    let (_, key, generation, ticket, trust) = fixture();
    let mut payload = ticket.encode();
    payload.splice(1..2, [24, 1]);
    assert_eq!(
        Ticket::verify(&trust::sign(TICKET_DOMAIN, &payload, &key), &trust, NOW),
        Err(Error::Encoding)
    );
    let invalid = Ticket {
        entitlement: Entitlement {
            valid_until: NOW + ENTITLEMENT_LIFETIME + 1,
            ..ticket.entitlement
        },
        ..ticket
    };
    assert_eq!(invalid.issue(generation, &key), Err(Error::Lifetime));
}
