use super::*;
use windshare_relay_core::{
    flow::{Counts, DeliveryConsumed, SettlementAck, SettlementNotice},
    identifiers::RelaySessionId,
    sessions::OpaqueHeader,
};

fn join(
    host: &mut Memory,
    runtime: &mut Runtime<()>,
    f: &Fixture,
    receiver: u64,
) -> RelaySessionId {
    host.open(runtime, receiver);
    host.frame(runtime, f, receiver, "joinB64");
    let descriptor = host
        .sent
        .iter()
        .rev()
        .find(|(target, bytes)| *target == connection(receiver) && bytes.starts_with(b"WS2D"))
        .unwrap()
        .1
        .clone();
    assert_eq!(
        &descriptor[20..],
        host.stored.as_ref().unwrap().descriptor().unwrap()
    );
    RelaySessionId::new(descriptor[8..16].try_into().unwrap()).unwrap()
}

fn opaque(session: RelaySessionId, size: u32) -> Vec<u8> {
    let mut bytes = b"WS2O\x02\0\0\0".to_vec();
    bytes.extend_from_slice(session.as_bytes());
    bytes.extend_from_slice(&size.to_be_bytes());
    bytes.resize(20 + size as usize, 0xa5);
    bytes
}

fn forward(
    host: &mut Memory,
    runtime: &mut Runtime<()>,
    source: u64,
    session: RelaySessionId,
    size: u32,
) {
    runtime
        .message(
            host,
            connection(source),
            opaque(session, size),
            crate::runtime::RegistrationAccess::Denied,
        )
        .unwrap();
}

fn admit(host: &mut Memory, runtime: &mut Runtime<()>, f: &Fixture, session: RelaySessionId) {
    let mut bytes = f.frame("sessionAdmittedB64");
    bytes[8..16].copy_from_slice(session.as_bytes());
    runtime
        .control(
            host,
            connection(1),
            &bytes,
            crate::runtime::RegistrationAccess::Denied,
        )
        .unwrap();
}

fn consume(
    host: &mut Memory,
    runtime: &mut Runtime<()>,
    destination: u64,
    session: RelaySessionId,
    total: Counts,
) {
    let bytes = DeliveryConsumed::new(session, total).unwrap().encode();
    runtime
        .control(
            host,
            connection(destination),
            &bytes,
            crate::runtime::RegistrationAccess::Denied,
        )
        .unwrap();
}

fn grants(host: &Memory, source: u64, session: RelaySessionId) -> Counts {
    host.sent
        .iter()
        .filter(|(target, bytes)| {
            *target == connection(source)
                && bytes.starts_with(b"WS2W")
                && bytes[8..16] == *session.as_bytes()
        })
        .fold(Counts::ZERO, |total, (_, bytes)| {
            Counts::new(
                total.frames + u64::from(u32::from_be_bytes(bytes[16..20].try_into().unwrap())),
                total.encoded_bytes
                    + u64::from(u32::from_be_bytes(bytes[20..24].try_into().unwrap())),
            )
        })
}

#[test]
fn bidirectional_windows_survive_hibernation_without_sql_or_replay_credit() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    host.registered(&mut runtime, &f);
    let session = join(&mut host, &mut runtime, &f, 2);
    let mut writes = (host.writes, host.manifest_writes);
    let initial = [grants(&host, 1, session), grants(&host, 2, session)];
    let mut total = Counts::ZERO;
    for batch in 0..12 {
        for size in [1, OpaqueHeader::MAX_ENCODED_BYTES - 20] {
            for source in [2, 1] {
                let destination = if source == 1 { 2 } else { 1 };
                let bytes = opaque(session, size);
                let before = host.sent.len();
                forward(&mut host, &mut runtime, source, session, size);
                assert!(host.sent[before..].contains(&(connection(destination), bytes)));
            }
            total.frames += 1;
            total.encoded_bytes += u64::from(size + 20);
            if total.frames == 1 {
                admit(&mut host, &mut runtime, &f, session);
            }
        }
        // Both complete envelopes still occupy the full window before consumption.
        let before = [grants(&host, 1, session), grants(&host, 2, session)];
        assert_eq!((host.writes, host.manifest_writes), writes);
        drop(runtime);
        host.time.0 += 10;
        runtime = host.restore(u8::try_from(batch + 2).unwrap()).unwrap();
        writes = (host.writes, host.manifest_writes);
        assert_eq!(
            [grants(&host, 1, session), grants(&host, 2, session)],
            before
        );
        for destination in [1, 2] {
            consume(&mut host, &mut runtime, destination, session, total);
        }
        let after = [grants(&host, 1, session), grants(&host, 2, session)];
        for (index, count) in after.iter().enumerate() {
            assert_eq!(
                *count,
                Counts::new(
                    initial[index].frames + total.frames,
                    initial[index].encoded_bytes + total.encoded_bytes
                )
            );
        }
        for destination in [1, 2] {
            consume(&mut host, &mut runtime, destination, session, total);
        }
        assert_eq!(
            [grants(&host, 1, session), grants(&host, 2, session)],
            after
        );
        assert_eq!((host.writes, host.manifest_writes), writes);
    }
}

#[test]
fn stop_orders_exact_submitted_prefix_before_closing_both_destinations() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    host.time = f.time("stop");
    host.seed = Some(f.seed("stop"));
    let mut runtime = host.restore(1).unwrap();
    host.open(&mut runtime, 99);
    host.frame(&mut runtime, &f, 99, "stopInitB64");
    host.time = f.time("");
    host.seed = Some(f.seed(""));
    host.registered(&mut runtime, &f);
    let session = join(&mut host, &mut runtime, &f, 2);
    for source in [2, 1] {
        forward(&mut host, &mut runtime, source, session, 1);
    }
    host.frame(&mut runtime, &f, 99, "stopProofB64");
    for destination in [1, 2] {
        let notice = host
            .sent
            .iter()
            .find(|(target, bytes)| {
                *target == connection(destination) && bytes.starts_with(b"WS2N")
            })
            .expect("settlement must be sent while destination is still open");
        assert_eq!(
            SettlementNotice::decode(&notice.1).unwrap().total(),
            Counts::new(1, 21)
        );
        assert!(!host.live.contains(&connection(destination)));
    }
    assert!(matches!(
        host.stored.as_ref().unwrap().record(),
        Record::Stopped(_)
    ));
    let before = host.sent.len();
    forward(&mut host, &mut runtime, 1, session, 1);
    assert!(
        !host.sent[before..]
            .iter()
            .any(|(_, bytes)| bytes.starts_with(b"WS2O"))
    );
}

#[test]
fn cancellation_settles_without_closing_the_shared_sender() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    host.registered(&mut runtime, &f);
    let session = join(&mut host, &mut runtime, &f, 2);
    forward(&mut host, &mut runtime, 2, session, 1);
    runtime
        .event(
            &mut host,
            Input::Cancel {
                participant: connection(2),
                session,
            },
        )
        .unwrap();
    let ack = SettlementAck::new(session, Counts::new(1, 21))
        .unwrap()
        .encode();
    runtime
        .control(
            &mut host,
            connection(1),
            &ack,
            crate::runtime::RegistrationAccess::Denied,
        )
        .unwrap();
    runtime
        .control(
            &mut host,
            connection(1),
            &ack,
            crate::runtime::RegistrationAccess::Denied,
        )
        .unwrap();
    assert!(host.live.contains(&connection(1)));
    let next = join(&mut host, &mut runtime, &f, 3);
    forward(&mut host, &mut runtime, 3, next, 1);
    assert!(host.sent.contains(&(connection(1), opaque(next, 1))));
}

#[test]
fn receiver_loss_and_hibernation_discard_late_ingress_without_closing_siblings() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    host.registered(&mut runtime, &f);
    let retired = join(&mut host, &mut runtime, &f, 2);
    let healthy = join(&mut host, &mut runtime, &f, 3);
    forward(&mut host, &mut runtime, 2, retired, 1);
    host.close(connection(2)).unwrap();
    runtime
        .event(&mut host, Input::Disconnected(connection(2)))
        .unwrap();
    let ack = SettlementAck::new(retired, Counts::new(1, 21))
        .unwrap()
        .encode();
    for boot in 1..=3 {
        if boot == 2 {
            runtime
                .control(
                    &mut host,
                    connection(1),
                    &ack,
                    crate::runtime::RegistrationAccess::Denied,
                )
                .unwrap();
        }
        if boot == 3 {
            drop(runtime);
            runtime = host.restore(boot).unwrap();
        }
        let before = host.sent.len();
        let writes = (host.writes, host.manifest_writes);
        forward(&mut host, &mut runtime, 1, retired, 1);
        admit(&mut host, &mut runtime, &f, retired);
        assert_eq!(
            host.sent.len(),
            before,
            "discarded inputs cannot forward or grant credit"
        );
        assert_eq!((host.writes, host.manifest_writes), writes);
        assert!(host.live.contains(&connection(1)));
        assert!(host.live.contains(&connection(3)));
        for source in [3, 1] {
            let destination = if source == 1 { 3 } else { 1 };
            let before = host.sent.len();
            forward(&mut host, &mut runtime, source, healthy, 1);
            assert!(host.sent[before..].contains(&(connection(destination), opaque(healthy, 1))));
            consume(
                &mut host,
                &mut runtime,
                destination,
                healthy,
                Counts::new(u64::from(boot), u64::from(boot) * 21),
            );
        }
        admit(&mut host, &mut runtime, &f, healthy);
    }
}

#[test]
fn uncertain_payload_handoff_retires_only_its_destination_and_dependent_flow() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    host.registered(&mut runtime, &f);
    let failed = join(&mut host, &mut runtime, &f, 2);
    let healthy = join(&mut host, &mut runtime, &f, 3);
    host.fail_send = Some(connection(2));
    forward(&mut host, &mut runtime, 1, failed, 1);
    assert!(!host.live.contains(&connection(2)));
    assert!(host.live.contains(&connection(1)));
    forward(&mut host, &mut runtime, 3, healthy, 1);
    assert!(host.sent.contains(&(connection(1), opaque(healthy, 1))));
    drop(runtime);
    let mut runtime = host.restore(2).unwrap();
    forward(&mut host, &mut runtime, 1, healthy, 1);
    assert!(host.sent.contains(&(connection(3), opaque(healthy, 1))));
}

#[test]
fn lost_inventory_never_becomes_first_creation_with_or_without_authority_or_sockets() {
    let f = Fixture::new();
    for published in [false, true] {
        for sockets in [false, true] {
            let mut host = Memory::new(&f);
            let mut runtime = host.restore(1).unwrap();
            if published {
                host.registered(&mut runtime, &f);
                let session = join(&mut host, &mut runtime, &f, 2);
                forward(&mut host, &mut runtime, 2, session, 1);
            } else {
                host.open(&mut runtime, 1);
            }
            drop(runtime);
            host.manifest = None;
            if !sockets {
                host.live.clear();
                host.attachments.clear();
            }
            let before = host.sent.len();
            assert!(host.restore(2).is_err());
            assert_eq!(host.sent.len(), before);
            assert!(host.manifest.is_none());
        }
    }
}

#[test]
fn malformed_or_overcredit_payloads_never_forward() {
    let f = Fixture::new();
    for oversized in [false, true] {
        let mut host = Memory::new(&f);
        let mut runtime = host.restore(1).unwrap();
        host.registered(&mut runtime, &f);
        let session = join(&mut host, &mut runtime, &f, 2);
        if !oversized {
            for _ in 0..config::delivery_window().frames {
                forward(&mut host, &mut runtime, 2, session, 1);
            }
        }
        let before = host.sent.len();
        let size = if oversized {
            OpaqueHeader::MAX_ENCODED_BYTES
        } else {
            1
        };
        forward(&mut host, &mut runtime, 2, session, size);
        assert!(!host.live.contains(&connection(2)));
        assert!(
            !host.sent[before..]
                .iter()
                .any(|(_, bytes)| bytes.starts_with(b"WS2O"))
        );
    }
}
