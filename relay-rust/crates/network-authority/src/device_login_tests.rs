use super::*;
use windshare_relay_access::device_login::{Approval, Invitation};

#[test]
fn browser_session_authorizes_two_enrolled_nodes_with_independent_receipts() {
    let mut service = fixture();
    let now_ms = service.environment.now() * 1000;
    let password = "example-service-password";
    let mut request = registration(1);
    request.username = Some("sender".into());
    request.password = Some(password.into());
    service.register("source", request).unwrap();
    let (account, cookie) = service
        .device_sign_in("source", "sender", password)
        .unwrap();
    let generation = service.signing.generation;
    for byte in [2, 3] {
        let signer = SigningKey::from_bytes(&[byte; 32]);
        let node = signer.verifying_key().to_bytes();
        let invite = Invitation {
            node,
            issuer: generation.issuer,
            enrollment: [byte; 16],
            origin: format!("https://relay{byte}.example"),
            device: "Laptop".into(),
            code: "ABCD-EFGH".into(),
            expires_ms: now_ms + 60000,
        };
        let invitation_token = invite.issue(&signer).unwrap();
        assert!(service.device_invitation(&invitation_token).is_err());
        save(
            &mut service.store,
            &key("node", &node),
            &Node {
                key: node,
                zone: Zone::Global,
                region: "region".into(),
                enabled: true,
                anonymous: false,
                version: 1,
            },
        )
        .unwrap();
        let preview = service.device_invitation(&invitation_token).unwrap();
        let current = service.browser_account(&cookie).unwrap();
        let token = service.approve_device(&preview, &current).unwrap();
        let grant = Approval::verify(&token, generation, node, now_ms).unwrap();
        assert_eq!(
            (grant.account, grant.enrollment),
            (account.id, invite.enrollment)
        );
        assert!(Approval::verify(&token, generation, [0; 32], now_ms).is_err());
        assert!(Approval::verify(&token, generation, node, invite.expires_ms).is_err());
    }
    service
        .set_password(
            &URL_SAFE_NO_PAD.encode([1; 32]),
            "sender",
            "changed-service-password",
        )
        .unwrap();
    assert!(service.browser_account(&cookie).is_none());
}
