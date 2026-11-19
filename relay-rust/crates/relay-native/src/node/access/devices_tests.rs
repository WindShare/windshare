use super::*;

#[test]
fn replacement_index_observes_uses_after_preparation() {
    let key = Credential {
        principal: [1; 16],
        id: [2; 16],
        name: "device".into(),
        digest: Digest::of(b"device-key"),
        created_at: 1,
        last_used_at: None,
        state: CredentialState::Active,
    };
    let keys = [key.clone()];
    let devices = DeviceKeys::default();
    let previous = CredentialIndex::build(&keys, &devices, &CredentialIndex::default());
    let replacement = CredentialIndex::build(&keys, &devices, &previous);
    previous.get(key.digest).unwrap().record_use(2);
    assert_eq!(
        replacement.by_id(key.id).unwrap().snapshot().last_used_at,
        Some(2)
    );
}
