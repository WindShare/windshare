use super::*;

#[test]
fn claimant_receives_the_approval_location_and_a_recoverable_domain_separated_key() {
    let secret = [7; 32];
    let code = user_code([0, 1, 2, 3, 4, 5, 6, 31]);
    let enrollment =
        Enrollment::new([3; 16], code, "Laptop".into(), Digest::of(&secret), 1).unwrap();
    let view = enrollment_response(&enrollment, "https://console.example");
    assert_eq!(
        view["verification_uri_complete"],
        "https://console.example/?code=ABCD-EFG9"
    );
    assert_eq!(view["enrollment_id"], text_id(&enrollment.id));
    assert_eq!(view["expires_at"], enrollment.expires_at);
    let key = claimed_key(&secret);
    assert_eq!(key, claimed_key(&secret));
    assert_ne!(key, claimed_key(&[8; 32]));
    assert_ne!(key, format!("wsak_{}", text_id(&enrollment.claim_digest.0)));
}

#[test]
fn manual_export_is_scoped_to_one_relay_and_public_inventory_excludes_its_secret() {
    let key = claimed_key(&[2; 32]);
    let credential = Credential {
        id: [1; 16],
        principal: [2; 16],
        name: "Build host".into(),
        digest: Digest::of(key.as_bytes()),
        created_at: 12,
        last_used_at: None,
        state: CredentialState::Revoked { at: 14 },
    };
    assert_eq!(
        credential_view(&credential),
        json!({
            "credential_id":text_id(&credential.id),"principal_id":text_id(&credential.principal),
            "name":"Build host","created_at":12,"last_used_at":null,"revoked_at":14,
        })
    );
    assert_eq!(
        service_configuration("https://relay.example", &key),
        json!({"url":"https://relay.example", "access_key":key})
    );
}

#[test]
fn asset_cache_control_marks_only_hashed_bundles_immutable() {
    assert_eq!(
        asset_cache_control("/assets/index-1234.js"),
        IMMUTABLE_CACHE_CONTROL
    );
    assert_eq!(
        asset_cache_control("/assets/layout-1234.css"),
        IMMUTABLE_CACHE_CONTROL
    );
    for path in ["/", "/device/complete", "/third-party-licenses.txt"] {
        assert!(
            asset(path).is_some(),
            "expected embedded console asset {path}"
        );
        assert_eq!(asset_cache_control(path), NO_STORE_CACHE_CONTROL);
    }
}
