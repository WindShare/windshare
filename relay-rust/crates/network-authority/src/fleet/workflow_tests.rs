use super::*;
use crate::fleet::{
    NodeState,
    publication::{Publication, Publisher, Release},
    signature::Envelope,
    topology::Spec,
};
use ed25519_dalek::Signer;
use serde_json::{Value, json};

fn signed(key: &SigningKey, domain: &[u8], body: &Value) -> Value {
    let data = serde_json::to_vec(body).unwrap();
    json!({"payload":URL_SAFE_NO_PAD.encode(&data),
        "signature":URL_SAFE_NO_PAD.encode(key.sign(&[domain, &data].concat()).to_bytes())})
}
fn spec(alias: &str, byte: u8) -> Value {
    json!({"alias":alias, "relay":format!("https://{alias}.example"),
        "node_key":URL_SAFE_NO_PAD.encode([byte;32]), "roles":["relay"], "zone":"cn",
        "region":"shanghai", "operator":"official","provider":"isp","capacity_mbps":20})
}
fn directory() -> (Release, Publisher, String) {
    let root = SigningKey::from_bytes(&[5; 32]);
    let online = SigningKey::from_bytes(&[6; 32]);
    let mut cert = signed(
        &root,
        b"windshare/network-certificate/v1\0",
        &json!({
        "format":1,"network":"test","generation":1,"key":URL_SAFE_NO_PAD.encode(online.verifying_key().to_bytes()),
        "not_before":"1970-01-01T00:00:00Z","expires_at":"1970-05-01T00:00:00Z"}),
    );
    cert["root"] = json!("test-root");
    let snapshot = |alias, serial| {
        let mut entry = spec(alias, 9);
        entry["relay"] = json!(format!("https://{alias}.example/"));
        entry["status"] = json!("active");
        let mut envelope = signed(
            &online,
            b"windshare/network-directory/v1\0",
            &json!({
            "format":1,"network":"test","generation":1,"serial":serial,
            "issued_at":"1970-01-01T00:00:00Z","expires_at":"1970-01-05T00:00:00Z","nodes":[entry],
            "public_stun":[{"url":"stun:stun.example:3478","provider":"isp","region":"cn","failure_domain":"test"}]}),
        );
        envelope["certificate"] = cert.clone();
        envelope
    };
    let history = snapshot("retired", 3).to_string();
    let release: Release = serde_json::from_value(json!({"network":"test","roots":[{"id":"test-root",
        "key":URL_SAFE_NO_PAD.encode(root.verifying_key().to_bytes())}],"snapshot":snapshot("prior",4),
        "link_host":"https://ws.ourworld.top","backup_entry":"https://windshare.cc"})).unwrap();
    let signer = Publisher::new(
        &release,
        serde_json::from_value(cert).unwrap(),
        &URL_SAFE_NO_PAD.encode(online.to_bytes()),
    )
    .unwrap();
    (release, signer, history)
}
#[test]
fn importing_seeds_listed_nodes_alias_bindings_and_renews_only_once() {
    let mut auth = fixture();
    let admin = auth
        .initialize("init", "init", "operator", "reasonable password")
        .unwrap();
    let (release, signer, history) = directory();
    let mut publication = Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
    let imported: Node = load(&auth.store, &key("node", &[9; 32])).unwrap().unwrap();
    assert!(
        imported.admission() == windshare_relay_access::usage::Admission::Accepting
            && !imported.anonymous
            && imported.version == 1
            && imported.spec.zone == Zone::Cn
            && matches!(imported.state(), NodeState::Active)
    );
    assert_eq!(publication.revision().unwrap().serial, 4);
    let mut candidate: Spec = serde_json::from_value(spec("new", 7)).unwrap();
    candidate.alias = "retired".into();
    assert!(auth.enroll(&admin, candidate, &signer).is_err());
    let now = auth.environment.now();
    assert!(publication.renew(&signer, now).unwrap());
    assert!(!publication.renew(&signer, now).unwrap());
}

#[path = "../host/fleet/route_tests.rs"]
mod fleet_route;
#[path = "operations_tests.rs"]
mod operations;
#[path = "../host/fleet/public_directory_tests.rs"]
mod public_directory;
#[path = "../host/fleet/reconcile_tests.rs"]
mod reconcile;
