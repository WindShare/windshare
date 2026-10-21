use super::*;

#[test]
fn full_restart_preserves_manifest_risk_expiry_even_after_host_policy_changes() {
    use std::{collections::VecDeque, time::Duration};
    use windshare_relay_core::share::{
        Limits, Output, Responsibility, Restore, RetirementPolicy, ShareRelay, WorkResult,
    };

    const RETENTION: Duration = Duration::from_millis(200);
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    host.registered(&mut runtime, &f);
    drop(runtime);
    host.live.clear();
    host.attachments.clear();
    let limits = Limits {
        retirement: RetirementPolicy::Bounded {
            admission_grace: Duration::ZERO,
            risk_retention: RETENTION,
            generations: config::MAX_BUSINESS_CONNECTIONS,
            residual: config::limits().delivery.total(),
        },
        ..config::limits()
    };
    let manifest = CheckpointManifest::decode(host.manifest.as_ref().unwrap(), limits).unwrap();
    let checkpoint = Checkpoint::from_connections(&manifest, Vec::new(), limits).unwrap();
    let (mut kernel, turn) = ShareRelay::restore(
        f.identity(2),
        limits,
        &Restore::restarted(
            host.stored.as_ref().map(StoredAuthority::record),
            Responsibility::Checkpoint(&checkpoint),
        ),
        (),
        host.time,
    )
    .unwrap();
    let mut outputs: VecDeque<_> = turn.outputs.into();
    while let Some(output) = outputs.pop_front() {
        if let Output::Work(work) = output {
            host.execute(&work.operation).unwrap();
            outputs.extend(
                kernel
                    .handle(
                        Input::Completed {
                            id: work.id,
                            result: WorkResult::Complete,
                        },
                        host.time,
                    )
                    .outputs,
            );
        }
    }
    host.manifest = Some(kernel.checkpoint_manifest().unwrap().encode());
    let expiry = host.time.saturating_add(RETENTION);
    for boot in 3..=5 {
        host.time.0 += 10;
        let runtime = host.restore(boot).unwrap();
        assert_eq!(runtime.deadline(), Some(expiry));
        let manifest =
            CheckpointManifest::decode(host.manifest.as_ref().unwrap(), config::limits()).unwrap();
        assert_eq!(manifest.generations().collect::<Vec<_>>(), [connection(1)]);
    }
    // The alarm/input path and cold recovery both use the original persisted deadline.
    host.time = expiry;
    let mut runtime = host.restore(6).unwrap();
    runtime.event(&mut host, Input::Tick).unwrap();
    let manifest =
        CheckpointManifest::decode(host.manifest.as_ref().unwrap(), config::limits()).unwrap();
    assert_eq!(manifest.generations().count(), 0);
}

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
    for (write, committed) in [(2, true), (3, false), (3, true)] {
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
            control(
                &mut runtime,
                &mut host,
                connection(2),
                &f.frame("stopProofB64"),
                windshare_relay_core::access::RegistrationDecision::Denied
            )
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
