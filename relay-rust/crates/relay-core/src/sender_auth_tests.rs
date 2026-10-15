extern crate std;

use std::{string::String, vec::Vec};

use serde::Deserialize;

use super::{InvalidPublicKey, InvalidSignature, SenderPublicKey};

const SHARED_VECTORS: &str = include_str!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../../../core/testvectors/ed25519-acceptance.json"
));

#[derive(Deserialize)]
struct AcceptanceVectors {
    version: u32,
    kind: String,
    cases: Vec<AcceptanceCase>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct AcceptanceCase {
    name: String,
    public_key_hex: String,
    message_hex: String,
    signature_hex: String,
    accepted: bool,
    key_accepted: bool,
}

fn vectors() -> Vec<AcceptanceCase> {
    let vectors: AcceptanceVectors =
        serde_json::from_str(SHARED_VECTORS).expect("shared acceptance vectors must parse");
    assert_eq!(vectors.version, 1);
    assert_eq!(vectors.kind, "ed25519-acceptance");
    assert!(!vectors.cases.is_empty(), "shared cases must not be empty");
    vectors.cases
}

fn bytes(value: &str) -> Vec<u8> {
    hex::decode(value).expect("shared vector bytes must be hex")
}

#[test]
fn public_keys_match_shared_acceptance() {
    for case in vectors() {
        let encoded = bytes(&case.public_key_hex);
        let key = SenderPublicKey::from_bytes(&encoded);
        assert_eq!(key.is_ok(), case.key_accepted, "key: {}", case.name);
        if let Ok(key) = key {
            assert_eq!(key.to_bytes().as_slice(), encoded, "key: {}", case.name);
        }
    }
}

#[test]
fn signatures_match_shared_acceptance() {
    for case in vectors() {
        let key = SenderPublicKey::from_bytes(&bytes(&case.public_key_hex));
        let accepted = key.is_ok_and(|key| {
            key.verify(&bytes(&case.message_hex), &bytes(&case.signature_hex))
                .is_ok()
        });
        assert_eq!(accepted, case.accepted, "signature: {}", case.name);
    }
}

#[test]
fn retained_key_owns_its_identity_and_survives_rejected_signatures() {
    let case = vectors()
        .into_iter()
        .find(|case| case.name == "valid-97")
        .expect("shared positive vector");
    let mut input = bytes(&case.public_key_hex);
    let key = SenderPublicKey::from_bytes(&input).expect("valid sender key");
    let mut exported = key.to_bytes();
    input.fill(0);
    exported.fill(0);
    assert_eq!(SenderPublicKey::from_bytes(&input), Err(InvalidPublicKey));
    assert_eq!(
        SenderPublicKey::from_bytes(&exported),
        Err(InvalidPublicKey)
    );

    let message = bytes(&case.message_hex);
    let signature = bytes(&case.signature_hex);
    assert_eq!(key.verify(&message, &signature), Ok(()));
    assert_eq!(
        key.verify(b"another operation", &signature),
        Err(InvalidSignature)
    );
    assert_eq!(key.verify(&message, &signature), Ok(()));
    assert_eq!(key.to_bytes().as_slice(), bytes(&case.public_key_hex));
}
