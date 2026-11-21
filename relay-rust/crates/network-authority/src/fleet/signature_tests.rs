use super::{Envelope, Root};
use serde::Deserialize;

#[derive(Deserialize)]
struct Vectors {
    network: String,
    roots: Vec<Root>,
    entries: Vec<Case>,
}
#[derive(Deserialize)]
struct Case {
    name: String,
    valid: bool,
    envelope: Envelope,
}

#[test]
fn signed_directory_agrees_with_native_and_browser() {
    let vectors: Vectors = serde_json::from_str(include_str!(
        "../../../../../testdata/network/directory.json"
    ))
    .unwrap();
    for case in vectors.entries {
        let result = case
            .envelope
            .verify(&vectors.network, &vectors.roots, false);
        assert_eq!(
            result.is_ok(),
            case.valid,
            "{}: {:?}",
            case.name,
            result.err()
        );
    }
}
