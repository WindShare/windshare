use super::*;
use windshare_relay_access::{
    restrictions::{self, PAGE_ENTRIES, PageRequest, PageResult, Restriction, Restrictions},
    usage::MAX_REPORT_BYTES,
};

fn requested_page(
    authority: &Authority<Memory, Clock>,
    node: &SigningKey,
    version: u64,
    after: Option<Restriction>,
) -> PageResult {
    let request = PageRequest {
        id: [23; 16],
        issuer: authority.signing.generation.issuer,
        node: node.verifying_key().to_bytes(),
        version,
        after,
    };
    let signed = trust::sign(
        restrictions::REQUEST_DOMAIN,
        &serde_json::to_vec(&request).unwrap(),
        node,
    );
    let response = authority.restriction_page(&signed).unwrap();
    assert!(serde_json::to_vec(&response).unwrap().len() <= MAX_REPORT_BYTES);
    let body = URL_SAFE_NO_PAD.decode(response.body).unwrap();
    let reply: Reply<PageResult> = serde_json::from_slice(
        trust::verify(
            restrictions::RESPONSE_DOMAIN,
            &body,
            &authority.signing.generation.key,
        )
        .unwrap(),
    )
    .unwrap();
    assert_eq!(reply.request, request.id);
    reply.result
}

#[test]
fn oversized_restrictions_paginate_without_blocking_usage_or_status() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    let node = SigningKey::from_bytes(&[3; 32]);
    authority
        .enroll_node(
            &admin,
            &NodeEnrollment {
                key: node.verifying_key().to_bytes(),
                zone: Zone::Global,
                region: "test".into(),
            },
        )
        .unwrap();
    let session = authority.register("source", registration(1)).unwrap();
    let mut expected = Restrictions::default();
    for i in 0..8192u16 {
        let mut id = [255; 16];
        id[..2].copy_from_slice(&i.to_be_bytes());
        authority
            .set_restriction(Restriction::Credential(id), true)
            .unwrap();
        expected.credentials.insert(id);
    }
    authority
        .set_restriction(Restriction::Share([91; 12]), true)
        .unwrap();
    expected.shares.insert([91; 12]);
    authority
        .set_restriction(Restriction::Account([92; 16]), true)
        .unwrap();
    expected.accounts.insert([92; 16]);
    let version = authority.restriction_version().unwrap();
    expected.version = version;
    assert!(
        URL_SAFE_NO_PAD
            .encode(serde_json::to_vec(&expected).unwrap())
            .len()
            > MAX_REPORT_BYTES
    );

    let usage = batch(&authority, &node, session.account, 1, 2, 300);
    assert!(matches!(
        reply(&mut authority, &usage),
        ReportResult::UsageReceived
    ));
    let refresh = batch(&authority, &node, session.account, 2, 2, 0);
    assert!(matches!(
        reply(&mut authority, &refresh),
        ReportResult::Status {
            restriction_version,
            ..
        } if restriction_version == version
    ));

    let mut restored = Restrictions {
        version,
        ..Restrictions::default()
    };
    let mut after = None;
    loop {
        let PageResult::Page { entries, more } = requested_page(&authority, &node, version, after)
        else {
            panic!("unchanged version must be pageable");
        };
        assert!(entries.len() <= PAGE_ENTRIES);
        after = entries.last().copied();
        for entry in entries {
            assert!(restored.insert(entry));
        }
        if !more {
            break;
        }
    }
    assert_eq!(restored, expected);

    authority
        .set_restriction(Restriction::Share([91; 12]), false)
        .unwrap();
    assert!(matches!(
        requested_page(&authority, &node, version, after),
        PageResult::Superseded { version: next } if next == version + 1
    ));
}
