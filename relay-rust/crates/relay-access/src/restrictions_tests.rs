use super::*;

#[test]
fn credential_revocation_is_issuer_scoped_and_does_not_block_sibling_credentials() {
    let mut restrictions = Restrictions {
        version: 3,
        ..Restrictions::default()
    };
    restrictions.credentials.insert([2; 16]);
    let attribution = Attribution::Account {
        issuer: [1; 32],
        account: [1; 16],
        credential: [2; 16],
    };
    assert!(restrictions.denied([1; 32], [1; 12], attribution));
    assert!(!restrictions.denied([9; 32], [1; 12], attribution));
    assert!(!restrictions.denied(
        [1; 32],
        [1; 12],
        Attribution::Account {
            issuer: [1; 32],
            account: [1; 16],
            credential: [3; 16]
        }
    ));
    assert!(!restrictions.advance(Restrictions {
        version: 2,
        ..Restrictions::default()
    }));
    assert!(restrictions.denied([1; 32], [1; 12], attribution));
    assert!(restrictions.advance(Restrictions {
        version: 4,
        ..Restrictions::default()
    }));
    assert!(!restrictions.denied([1; 32], [1; 12], attribution));
}
