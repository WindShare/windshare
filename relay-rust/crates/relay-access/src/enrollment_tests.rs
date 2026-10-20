use super::*;
use alloc::string::ToString;
fn request() -> Enrollment {
    Enrollment::new(
        [1; 16],
        "ABCD-EFGH".to_string(),
        "Laptop".to_string(),
        Digest::of(b"claim"),
        0,
    )
    .unwrap()
}

#[test]
fn approval_claim_retry_and_install_keep_one_credential() {
    let mut enrollment = request();
    assert_eq!(
        enrollment.poll(Digest::of(b"other"), 0),
        Err(Refusal::AccessDenied)
    );
    assert_eq!(enrollment.poll(Digest::of(b"claim"), 0), Ok(Poll::Pending));
    enrollment.approve([2; 16], 1).unwrap();
    enrollment.approve([3; 16], 2).unwrap();
    assert!(matches!(
        enrollment.poll(Digest::of(b"claim"), 2),
        Err(Refusal::SlowDown { .. })
    ));
    for at in [POLL_INTERVAL_MS, POLL_INTERVAL_MS * 2] {
        assert_eq!(
            enrollment.poll(Digest::of(b"claim"), at),
            Ok(Poll::Approved {
                credential: [2; 16]
            })
        );
    }
    enrollment
        .installed(Digest::of(b"claim"), POLL_INTERVAL_MS * 2)
        .unwrap();
    assert_eq!(enrollment.incomplete_credential(), None);
    enrollment
        .installed(Digest::of(b"claim"), ENROLLMENT_LIFETIME_MS + 1)
        .unwrap();
    assert_eq!(
        enrollment.installed(Digest::of(b"other"), ENROLLMENT_LIFETIME_MS + 1),
        Err(Refusal::AccessDenied)
    );
    assert_eq!(
        enrollment.cancel(Digest::of(b"other")),
        Err(Refusal::AccessDenied)
    );
    assert_eq!(enrollment.cancel(Digest::of(b"claim")), Ok(Some([2; 16])));
    assert_eq!(enrollment.cancel(Digest::of(b"claim")), Ok(None));
    assert_ne!(enrollment.installed(Digest::of(b"claim"), 1), Ok(()));
}

#[test]
fn cancellation_and_expiry_reclaim_only_incomplete_credentials() {
    let mut enrollment = request();
    enrollment.approve([2; 16], 0).unwrap();
    assert_eq!(enrollment.incomplete_credential(), Some([2; 16]));
    assert_eq!(
        enrollment.poll(Digest::of(b"claim"), ENROLLMENT_LIFETIME_MS),
        Err(Refusal::Expired)
    );
    assert_eq!(enrollment.cancel(Digest::of(b"claim")), Ok(Some([2; 16])));
    assert_eq!(enrollment.incomplete_credential(), None);
    assert_eq!(enrollment.approve([3; 16], 1), Err(Refusal::Cancelled));
}

#[test]
fn revoked_credentials_cannot_authorize_new_registrations() {
    use crate::{Attribution, Credential, CredentialState};
    let mut credential = Credential {
        principal: [1; 16],
        id: [2; 16],
        name: "Laptop".to_string(),
        digest: Digest::of(b"access"),
        created_at: 0,
        last_used_at: None,
        state: CredentialState::Active,
    };
    let attribution = credential.authorize(Digest::of(b"access"), 5).unwrap();
    assert_eq!(Attribution::decode(attribution.encode()), Some(attribution));
    assert_eq!(credential.last_used_at, Some(5));
    credential.revoke(6);
    assert_eq!(
        credential.authorize(Digest::of(b"access"), 7),
        Err(Refusal::AccessDenied)
    );
    assert_eq!(
        Attribution::decode(Attribution::Anonymous.encode()),
        Some(Attribution::Anonymous)
    );
}
