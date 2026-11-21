use super::*;
use crate::fleet::{
    ListingState,
    publication::{Publication, Publisher, Release, digest},
    signature::Envelope,
    topology::{Spec, Status},
    vantage::{Agent, Proof},
};
use ed25519_dalek::Signer;
use serde_json::{Value, json};
use windshare_relay_access::probe;

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
fn proof(key: &SigningKey, domain: &[u8], body: &Value) -> Proof {
    let mut value = signed(key, domain, body);
    value["key"] = json!(URL_SAFE_NO_PAD.encode(key.verifying_key().to_bytes()));
    serde_json::from_value(value).unwrap()
}

#[test]
fn importing_preserves_policies_alias_bindings_and_renews_only_once() {
    let mut auth = fixture();
    let admin = auth
        .initialize("init", "init", "operator", "reasonable password")
        .unwrap();
    let (release, signer, history) = directory();
    let prior = Node {
        listing: None,
        key: [9; 32],
        zone: Zone::Global,
        region: "prior".into(),
        enabled: false,
        anonymous: true,
        version: 19,
    };
    save(&mut auth.store, &key("node", &prior.key), &prior).unwrap();
    let mut publication = Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
    let imported: Node = load(&auth.store, &key("node", &prior.key))
        .unwrap()
        .unwrap();
    assert!(!imported.enabled && imported.anonymous && imported.version == 19);
    assert_eq!(publication.revision().unwrap().serial, 4);
    let mut candidate: Spec = serde_json::from_value(spec("new", 7)).unwrap();
    candidate.alias = "retired".into();
    assert!(auth.stage(&admin, candidate).is_err());
    let now = auth.environment.now();
    assert!(publication.renew(&signer, now).unwrap());
    assert!(!publication.renew(&signer, now).unwrap());
}

#[test]
fn a_verified_staging_node_requires_current_mirrors_before_publication() {
    let mut auth = fixture();
    let admin = auth
        .initialize("init", "init", "operator", "reasonable password")
        .unwrap();
    let (release, signer, history) = directory();
    let publication = Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
    let old_stun = publication.payload().unwrap().public_stun;
    let candidate: Spec = serde_json::from_value(spec("new", 7)).unwrap();
    let node = auth.stage(&admin, candidate.clone()).unwrap();
    assert_eq!(publication.payload().unwrap().nodes.len(), 1);
    let probe_key = SigningKey::from_bytes(&[8; 32]);
    let agent = Agent {
        id: "cn-a".into(),
        zone: Zone::Cn,
        key: URL_SAFE_NO_PAD.encode(probe_key.verifying_key().to_bytes()),
    };
    let agents = [agent];
    let now = auth.environment.now();
    let shares = [[1u8; 12], [2u8; 12], [3u8; 12]];
    let poll = |mirror: Option<String>, at| {
        proof(
            &probe_key,
            b"windshare/vantage-poll/v1\0",
            &json!({"at":at,"shares": shares, "mirror":mirror}),
        )
    };
    let origins = || {
        [
            release.link_host.clone(),
            release.backup_entry.clone(),
            "https://other.example".into(),
        ]
    };
    let start = poll(None, now);
    assert!(auth.claim(&[], &start, origins()).is_err());
    let work = auth.claim(&agents, &start, origins()).unwrap().unwrap();
    assert!(auth.claim(&agents, &start, origins()).is_err());
    assert_eq!(work.spec, candidate);
    assert_eq!(work.tickets.len(), shares.len());
    let mut trusted = TrustStore::default();
    trusted.accept(auth.signing.generation, now).unwrap();
    for (index, raw) in work.tickets.iter().enumerate() {
        let ticket = Ticket::verify(&URL_SAFE_NO_PAD.decode(raw).unwrap(), &trusted, now).unwrap();
        assert!(probe::permits(
            ticket,
            Some(&node.key),
            &[u8::try_from(index + 1).unwrap(); 12]
        ));
        assert!(!probe::permits(ticket, Some(&[10; 32]), &[1; 12]));
    }
    let promote = |auth: &mut Authority<Memory, Clock>, observed: &Envelope, enrolled: &[Agent]| {
        auth.promote(
            &admin,
            &candidate.node_key,
            Status::Canary,
            enrolled,
            observed,
            &signer,
        )
    };
    assert!(promote(&mut auth, &publication.envelope, &agents).is_err());
    let outcome = proof(
        &probe_key,
        b"windshare/vantage-result/v1\0",
        &json!({"at":now,"node_key":candidate.node_key,"lease":work.lease,"error":null}),
    );
    auth.complete_probe(&agents, &outcome).unwrap();
    assert!(auth.complete_probe(&agents, &outcome).is_err());
    assert!(promote(&mut auth, &publication.envelope, &agents).is_err());
    auth.environment.now += 1;
    assert!(
        auth.claim(
            &agents,
            &poll(
                Some(digest(&publication.envelope).unwrap()),
                auth.environment.now
            ),
            origins()
        )
        .unwrap()
        .is_none()
    );
    assert!(promote(&mut auth, &publication.envelope, &[]).is_err());
    promote(&mut auth, &publication.envelope, &agents).unwrap();
    let promoted = crate::fleet::publication::current(&auth.store).unwrap();
    let data = promoted
        .envelope
        .verify(&release.network, &release.roots, false)
        .unwrap();
    assert_eq!((data.generation, data.serial, data.nodes.len()), (1, 5, 2));
    assert_eq!(data.public_stun, old_stun);
    let saved: Node = load(&auth.store, &key("node", &node.key)).unwrap().unwrap();
    assert!(matches!(
        saved.listing.unwrap().state,
        ListingState::Listed {
            status: Status::Canary
        }
    ));
}
