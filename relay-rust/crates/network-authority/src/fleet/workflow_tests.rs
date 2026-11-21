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
fn proof(key: &SigningKey, domain: &[u8], body: &Value) -> Proof {
    let mut value = signed(key, domain, body);
    value["key"] = json!(URL_SAFE_NO_PAD.encode(key.verifying_key().to_bytes()));
    serde_json::from_value(value).unwrap()
}

fn cn_agent(key: &SigningKey) -> Agent {
    Agent {
        id: "cn-a".into(),
        zone: Zone::Cn,
        key: URL_SAFE_NO_PAD.encode(key.verifying_key().to_bytes()),
    }
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
    let agents = [cn_agent(&probe_key)];
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
            &crate::fleet::Promote {
                node_key: candidate.node_key.clone(),
                status: Status::Canary,
                operation_id: work.operation_id.clone(),
            },
            enrolled,
            &digest(observed).unwrap(),
            &signer,
        )
    };
    assert!(promote(&mut auth, &publication.envelope, &agents).is_err());
    let outcome = proof(
        &probe_key,
        b"windshare/vantage-result/v1\0",
        &json!({"at":now,"node_key":candidate.node_key,"lease":work.lease,"outcome":{"kind":"passed"}}),
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
#[test]
fn draft_conflicts_and_withdrawal_are_scoped_to_one_operation() {
    let mut auth = fixture();
    let admin = auth
        .initialize("init", "init", "operator", "reasonable password")
        .unwrap();
    let (release, _, history) = directory();
    Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
    let mut candidate: Spec = serde_json::from_value(spec("new", 7)).unwrap();
    let historic_endpoint = Spec {
        relay: "https://prior.example".into(),
        ..candidate.clone()
    };
    assert!(
        matches!(auth.stage(&admin, historic_endpoint).unwrap_err().downcast_ref::<Fault>(), Some(Fault::Conflict {reason}) if reason == "relay_bound")
    );
    candidate.stun = vec!["stun:stun.example:3478".into()];
    candidate.roles.push(crate::fleet::topology::Role::Stun);
    assert!(
        matches!(auth.stage(&admin,candidate.clone()).unwrap_err().downcast_ref::<Fault>(),Some(Fault::Conflict { reason }) if reason == "stun_bound")
    );
    candidate.stun = vec!["stun:new.example:3478".into()];
    let draft = auth
        .stage(&admin, candidate.clone())
        .unwrap()
        .listing
        .unwrap();
    let mut duplicate: Spec = serde_json::from_value(spec("other", 8)).unwrap();
    duplicate.stun.clone_from(&candidate.stun);
    duplicate.roles.clone_from(&candidate.roles);
    assert!(
        matches!(auth.stage(&admin, duplicate.clone()).unwrap_err().downcast_ref::<Fault>(), Some(Fault::Conflict {reason}) if reason == "stun_bound")
    );
    duplicate.relay.clone_from(&candidate.relay);
    assert!(
        matches!(auth.stage(&admin,duplicate).unwrap_err().downcast_ref::<Fault>(),Some(Fault::Conflict { reason }) if reason == "relay_bound")
    );
    auth.cancel_draft(&admin, &candidate.node_key, &draft.operation_id)
        .unwrap();
    candidate.relay = "https://correct.example".into();
    let next = auth
        .stage(&admin, candidate.clone())
        .unwrap()
        .listing
        .unwrap();
    assert_ne!(draft.operation_id, next.operation_id);
    assert!(
        auth.cancel_draft(&admin, &candidate.node_key, &draft.operation_id)
            .is_err()
    );
    auth.cancel_draft(&admin, &candidate.node_key, &next.operation_id)
        .unwrap();
    candidate.node_key = URL_SAFE_NO_PAD.encode([4; 32]);
    auth.stage(&admin, candidate).unwrap();
}

#[test]
fn failed_checks_back_off_and_a_replacement_draft_rejects_old_results() {
    let mut auth = fixture();
    let admin = auth
        .initialize("init", "init", "operator", "reasonable password")
        .unwrap();
    let (release, _, history) = directory();
    Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
    let candidate: Spec = serde_json::from_value(spec("retry", 7)).unwrap();
    let draft = auth
        .stage(&admin, candidate.clone())
        .unwrap()
        .listing
        .unwrap();
    let key = SigningKey::from_bytes(&[11; 32]);
    let agents = [cn_agent(&key)];
    let poll = |auth: &mut Authority<Memory, Clock>| {
        let shares = [[1u8; 12], [2u8; 12], [3u8; 12]];
        let proof = proof(
            &key,
            b"windshare/vantage-poll/v1\0",
            &json!({"at":auth.environment.now(), "shares":shares}),
        );
        auth.claim(&agents, &proof, Default::default()).unwrap()
    };
    let outcome = |at, lease: &str, kind: &str| {
        let outcome = if kind == "failed" {
            json!({"kind":kind, "reason":"timeout"})
        } else {
            json!({"kind":kind})
        };
        proof(
            &key,
            b"windshare/vantage-result/v1\0",
            &json!({"at":at, "node_key":candidate.node_key, "lease":lease, "outcome":outcome}),
        )
    };
    let mut work = poll(&mut auth).unwrap();
    for delay in [60, 120] {
        auth.complete_probe(
            &agents,
            &outcome(auth.environment.now(), &work.lease, "failed"),
        )
        .unwrap();
        auth.environment.now += delay - 1;
        assert!(poll(&mut auth).is_none());
        auth.environment.now += 1;
        work = poll(&mut auth).unwrap();
    }
    auth.cancel_draft(&admin, &candidate.node_key, &draft.operation_id)
        .unwrap();
    auth.stage(&admin, candidate.clone()).unwrap();
    auth.environment.now += 1;
    let replacement = poll(&mut auth).unwrap();
    assert_ne!(replacement.lease, work.lease);
    assert!(
        auth.complete_probe(
            &agents,
            &outcome(auth.environment.now(), &work.lease, "passed")
        )
        .is_err()
    );
    auth.complete_probe(
        &agents,
        &outcome(auth.environment.now(), &replacement.lease, "released"),
    )
    .unwrap();
    auth.environment.now += 1;
    assert!(poll(&mut auth).is_some());
}

#[test]
fn deployment_rejects_invalid_vantage_configuration_as_a_server_error() {
    use crate::fleet::vantage::validate_agents;
    let agent = Agent {
        id: "cn-a".into(),
        zone: Zone::Cn,
        key: URL_SAFE_NO_PAD.encode(SigningKey::from_bytes(&[11; 32]).verifying_key().to_bytes()),
    };
    validate_agents(std::slice::from_ref(&agent)).unwrap();
    for invalid in [
        Agent {
            key: "short".into(),
            ..agent.clone()
        },
        Agent {
            id: "two".into(),
            ..agent.clone()
        },
        agent.clone(),
    ] {
        let error = validate_agents(&[agent.clone(), invalid]).unwrap_err();
        assert!(error.downcast_ref::<Fault>().is_none());
    }
    for invalid in [
        json!({"key":agent.key,"id":"cn","zone":"CN"}),
        json!({"key":agent.key,"id":"cn","zone":"cn","extra":true}),
    ] {
        assert!(serde_json::from_value::<Agent>(invalid).is_err());
    }
}

#[test]
fn withdrawing_an_older_draft_releases_its_unpublished_alias_and_preserves_admission() {
    let mut auth = fixture();
    let admin = auth
        .initialize("init", "init", "operator", "reasonable password")
        .unwrap();
    Publication::load_or_import(&mut auth.store).unwrap();
    let mut candidate: Spec = serde_json::from_value(spec("old-draft", 7)).unwrap();
    let node = auth.stage(&admin, candidate.clone()).unwrap();
    let record = key("node", &node.key);
    let mut old_node = serde_json::to_value(&node).unwrap();
    old_node["listing"]["state"]
        .as_object_mut()
        .unwrap()
        .remove("enrollment");
    old_node["anonymous"] = json!(true);
    save(&mut auth.store, &record, &old_node).unwrap();
    let mut old_head: Value = load(&auth.store, "directory/current").unwrap().unwrap();
    old_head["aliases"][&candidate.alias] = json!("wss://old-draft.example/v2/ws");
    save(&mut auth.store, "directory/current", &old_head).unwrap();

    auth.cancel_draft(
        &admin,
        &candidate.node_key,
        &node.listing.unwrap().operation_id,
    )
    .unwrap();
    let saved: Node = load(&auth.store, &record).unwrap().unwrap();
    assert!(saved.listing.is_none() && saved.anonymous);
    candidate.relay = "https://replacement.example".into();
    auth.stage(&admin, candidate).unwrap();
}

#[test]
fn vantage_liveness_tracks_authority_receipt_time_separately_from_poll_replay_clock() {
    use crate::fleet::vantage::{SILENT_SECONDS, mainland_current, silent};

    let mut auth = fixture();
    let (release, _, history) = directory();
    let publication = Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
    let expected = digest(&publication.envelope).unwrap();
    let slow_key = SigningKey::from_bytes(&[12; 32]);
    let fast_key = SigningKey::from_bytes(&[13; 32]);
    let slow_agent = Agent {
        id: "cn-slow".into(),
        zone: Zone::Cn,
        key: URL_SAFE_NO_PAD.encode(slow_key.verifying_key().to_bytes()),
    };
    let fast_agent = Agent {
        id: "cn-fast".into(),
        zone: Zone::Cn,
        key: URL_SAFE_NO_PAD.encode(fast_key.verifying_key().to_bytes()),
    };
    let agents = [slow_agent.clone(), fast_agent.clone()];
    let shares = [[1u8; 12], [2u8; 12], [3u8; 12]];
    let poll = |key: &SigningKey, at: u64| {
        proof(
            key,
            b"windshare/vantage-poll/v1\0",
            &json!({"at": at, "shares": shares, "mirror": expected}),
        )
    };

    let t0 = auth.environment.now();
    // Slow agent reports 80 s behind authority; fast agent reports 80 s ahead.
    auth.claim(&agents, &poll(&slow_key, t0 - 80), Default::default())
        .unwrap();
    auth.claim(&agents, &poll(&fast_key, t0 + 80), Default::default())
        .unwrap();
    // Non-increasing client poll timestamp is still rejected against replay.
    assert!(
        auth.claim(&agents, &poll(&fast_key, t0 + 80), Default::default())
            .is_err()
    );
    assert!(
        auth.claim(&agents, &poll(&fast_key, t0 + 79), Default::default())
            .is_err()
    );

    // Near the end of the authority silence window, the slow agent is still live
    // even though now - poll.at already exceeds SILENT_SECONDS.
    let before_expiry = t0 + SILENT_SECONDS - 10;
    assert_eq!(
        silent(&auth.store, &agents, before_expiry).unwrap(),
        Vec::<String>::new()
    );
    assert!(mainland_current(&auth.store, &agents, &expected, before_expiry).unwrap());

    // Just past SILENT_SECONDS from authority receipt time, the fast agent is
    // also marked silent rather than getting an extra 80 s of self-reported grace.
    let after_expiry = t0 + SILENT_SECONDS + 1;
    assert_eq!(
        silent(&auth.store, &agents, after_expiry).unwrap(),
        vec![slow_agent.id, fast_agent.id]
    );
    assert!(!mainland_current(&auth.store, &agents, &expected, after_expiry).unwrap());
}
