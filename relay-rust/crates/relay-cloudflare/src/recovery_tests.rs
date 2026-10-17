use super::*;

#[test]
fn a_lost_attachment_does_not_replace_the_healthy_owner_or_renew_its_deadline() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    host.registered(&mut runtime, &f);
    let owner = host.stored.as_ref().unwrap().record();
    host.open(&mut runtime, 2);
    host.attachments.insert(connection(2), vec![0]);
    drop(runtime);
    let mut runtime = host.restore(2).unwrap();
    assert!(host.live.contains(&connection(1)));
    assert!(!host.live.contains(&connection(2)));
    assert_eq!(host.stored.as_ref().unwrap().record(), owner);
    host.frame(&mut runtime, &f, 1, "connectionProbeB64");
    assert_eq!(
        host.sent.last().unwrap().1,
        f.frame("connectionProbeAckB64")
    );
}

#[test]
fn uncertain_stop_receipts_cannot_erase_the_persisted_stop_intent() {
    let f = Fixture::new();
    for (write, committed) in [(3, true), (4, false), (4, true)] {
        let mut host = Memory::new(&f);
        host.time = f.time("stop");
        host.seed = Some(f.seed("stop"));
        let mut runtime = host.restore(1).unwrap();
        host.open(&mut runtime, 2);
        host.frame(&mut runtime, &f, 2, "stopInitB64");
        host.time = f.time("");
        host.seed = Some(f.seed(""));
        host.registered(&mut runtime, &f);
        host.fail_write = Some((write, committed));
        assert!(
            runtime
                .control(&mut host, connection(2), &f.frame("stopProofB64"), false)
                .is_err()
        );
        assert!(
            !host
                .sent
                .iter()
                .any(|(_, bytes)| bytes == &f.frame("stoppedB64"))
        );
        drop(runtime);
        host.live.clear();
        host.attachments.clear();
        host.fail_write = None;
        let _runtime = host.restore(2).unwrap();
        assert!(matches!(
            host.stored.as_ref().unwrap().record(),
            Record::Stopped(_)
        ));
        assert!(host.stored.as_ref().unwrap().descriptor().is_none());
    }
}

#[test]
fn authority_row_corruption_is_not_interpreted_as_absence() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    host.registered(&mut runtime, &f);
    let row = crate::persistence::AuthorityRow::new(host.stored.as_ref().unwrap());
    let mut encoded = serde_json::to_value(row).unwrap();
    encoded["descriptor"] = serde_json::Value::Null;
    let row: crate::persistence::AuthorityRow = serde_json::from_value(encoded).unwrap();
    assert!(
        row.decode(host.identity.relay, host.identity.share)
            .is_err()
    );
}
