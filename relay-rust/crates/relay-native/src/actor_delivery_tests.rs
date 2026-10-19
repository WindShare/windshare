use super::*;
use crate::connection::Writer;
use windshare_relay_core::{
    descriptor_upload::DescriptorUpload,
    flow::{Counts, DeliveryConsumed, SettlementAck, SettlementNotice},
    identifiers::RelaySessionId,
};

async fn published() -> Harness {
    let mut host = Harness::new().await;
    host.attempt().await.release.send(Receipt::Commit).unwrap();
    assert_eq!(&read(&mut host.sender).await[..4], b"WS2K");
    host
}

async fn joined(host: &mut Harness, id: u64) -> (Writer, RelaySessionId) {
    // The vector declares 16 frames: 14 ordinary plus this flow's startup
    // allowance, leaving one protected startup slot for a later receiver.
    const INITIAL_GRANTED_FRAMES: u64 = 15;
    let (port, mut receiver, _) = crate::connection::channel(connection(id));
    host.tx
        .send(Event::Connected(connection(id), port))
        .await
        .unwrap();
    host.tx
        .send(Event::Control(
            connection(id),
            host.fixture.bytes("joinB64"),
        ))
        .await
        .unwrap();
    let descriptor = read(&mut receiver).await;
    assert_eq!(&descriptor[..4], b"WS2D");
    let upload = host.fixture.bytes("descriptorUploadB64");
    assert_eq!(
        &descriptor[20..],
        DescriptorUpload::decode(&upload).unwrap().object()
    );
    let session = RelaySessionId::new(descriptor[8..16].try_into().unwrap()).unwrap();
    for writer in [&mut receiver, &mut host.sender] {
        let mut frames = 0;
        for _ in 0..2 {
            let grant = read(writer).await;
            assert_eq!(&grant[..4], b"WS2W");
            frames += u32::from_be_bytes(grant[16..20].try_into().unwrap());
        }
        assert_eq!(u64::from(frames), INITIAL_GRANTED_FRAMES);
    }
    (receiver, session)
}

fn opaque(session: RelaySessionId) -> Vec<u8> {
    let mut bytes = b"WS2O\x02\0\0\0".to_vec();
    bytes.extend_from_slice(session.as_bytes());
    bytes.extend_from_slice(&1_u32.to_be_bytes());
    bytes.push(7);
    bytes
}

#[tokio::test]
async fn stop_drops_accepted_queues_before_settlement_and_never_submits_their_tail() {
    let mut host = published().await;
    let (mut receiver, session) = joined(&mut host, 3).await;
    let frame = opaque(session);
    for _ in 0..4 {
        host.tx
            .send(Event::Frame(connection(1), frame.clone()))
            .await
            .unwrap();
    }
    host.stop().await;
    let stopping = host.attempt().await;
    assert!(matches!(
        stopping.operation,
        Storage::CompareExchange { .. }
    ));
    stopping.release.send(Receipt::Commit).unwrap();
    let stopped = host.attempt().await;
    // No writer readiness has been reported: only the two bounded submitted
    // slots may precede settlement; the accepted tail must have been discarded.
    for _ in 0..crate::config::SUBMITTED_DATA_FRAMES {
        assert_eq!(read(&mut receiver).await, frame);
    }
    let notice = SettlementNotice::decode(&read(&mut receiver).await).unwrap();
    assert_eq!(notice.total(), Counts::new(2, 42));
    assert_eq!(notice.session(), session);
    assert!(receiver.recv().await.is_none());
    let notice = SettlementNotice::decode(&read(&mut host.sender).await).unwrap();
    assert_eq!(notice.total(), Counts::ZERO);
    assert!(host.sender.recv().await.is_none());
    stopped.release.send(Receipt::Commit).unwrap();
    assert_eq!(&read(&mut host.stopper).await[..4], b"WS2Y");
    drop(host);
}

#[tokio::test]
async fn unrelated_ingress_is_isolated_and_wire_settlement_reclaims_shared_sender_capacity() {
    let mut host = published().await;
    let (receiver, session) = joined(&mut host, 3).await;
    let frame = opaque(session);
    let (intruder, mut refused, _) = crate::connection::channel(connection(4));
    host.tx
        .send(Event::Connected(connection(4), intruder))
        .await
        .unwrap();
    host.tx
        .send(Event::Frame(connection(4), frame.clone()))
        .await
        .unwrap();
    assert!(
        timeout(Duration::from_secs(3), refused.recv())
            .await
            .unwrap()
            .is_none()
    );
    for _ in 0..4 {
        host.tx
            .send(Event::Frame(connection(3), frame.clone()))
            .await
            .unwrap();
    }
    host.tx
        .send(Event::Disconnected(connection(3)))
        .await
        .unwrap();
    drop(receiver);
    for _ in 0..crate::config::SUBMITTED_DATA_FRAMES {
        assert_eq!(read(&mut host.sender).await, frame);
    }
    let notice = SettlementNotice::decode(&read(&mut host.sender).await).unwrap();
    assert_eq!(notice.total(), Counts::new(2, 42));
    let ack = SettlementAck::new(session, notice.total())
        .unwrap()
        .encode()
        .to_vec();
    host.tx
        .send(Event::Control(connection(1), ack))
        .await
        .unwrap();
    // Full grants in both directions prove the retained sender reclaimed its
    // submitted debt through WS2S, without closing it or writing delivery to SQL.
    let _ = joined(&mut host, 5).await;
    assert!(host.attempts.try_recv().is_err());
    drop(host);
}

#[tokio::test]
async fn receiver_loss_discards_late_data_and_admission_without_closing_shared_connections() {
    let mut host = published().await;
    let (receiver, retired) = joined(&mut host, 3).await;
    let (port, mut healthy, _) = crate::connection::channel(connection(4));
    host.tx
        .send(Event::Connected(connection(4), port))
        .await
        .unwrap();
    host.tx
        .send(Event::Control(connection(4), host.fixture.bytes("joinB64")))
        .await
        .unwrap();
    let descriptor = read(&mut healthy).await;
    let session = RelaySessionId::new(descriptor[8..16].try_into().unwrap()).unwrap();
    host.tx
        .send(Event::Frame(connection(3), opaque(retired)))
        .await
        .unwrap();
    // JOIN can leave replenishment controls ahead of the next payload.
    loop {
        let frame = read(&mut host.sender).await;
        if frame.starts_with(b"WS2O") {
            assert_eq!(frame, opaque(retired));
            break;
        }
        assert!(frame.starts_with(b"WS2W"));
    }
    host.tx
        .send(Event::Disconnected(connection(3)))
        .await
        .unwrap();
    drop(receiver);
    let notice = SettlementNotice::decode(&read(&mut host.sender).await).unwrap();
    assert_eq!(notice.session(), retired);
    for settled in [false, true] {
        if settled {
            let ack = SettlementAck::new(retired, notice.total())
                .unwrap()
                .encode()
                .to_vec();
            host.tx
                .send(Event::Control(connection(1), ack))
                .await
                .unwrap();
        }
        host.tx
            .send(Event::Frame(connection(1), opaque(retired)))
            .await
            .unwrap();
        let mut admitted = host.fixture.bytes("sessionAdmittedB64");
        admitted[8..16].copy_from_slice(retired.as_bytes());
        host.tx
            .send(Event::Control(connection(1), admitted))
            .await
            .unwrap();
        host.tx
            .send(Event::Frame(connection(1), opaque(session)))
            .await
            .unwrap();
        loop {
            let frame = read(&mut healthy).await;
            if frame.starts_with(b"WS2O") {
                assert_eq!(frame, opaque(session));
                break;
            }
            assert!(frame.starts_with(b"WS2W"));
        }
    }
    host.tx
        .send(Event::Frame(connection(4), opaque(session)))
        .await
        .unwrap();
    assert_eq!(read(&mut host.sender).await, opaque(session));
    assert!(
        host.attempts.try_recv().is_err(),
        "session loss must not persist owner detachment"
    );
    drop(host);
}

#[tokio::test]
async fn full_control_writer_defers_credit_and_readiness_resumes_without_double_grants() {
    let mut host = published().await;
    let (mut receiver, session) = joined(&mut host, 3).await;
    let frame = opaque(session);
    let controls = crate::config::CONTROL_QUEUE_FRAMES;
    // Consuming the first envelope releases startup, not ordinary credit.
    for index in 1..=controls + 2 {
        host.tx
            .send(Event::Frame(connection(1), frame.clone()))
            .await
            .unwrap();
        assert_eq!(read(&mut receiver).await, frame);
        let consumed = DeliveryConsumed::new(
            session,
            Counts::new(index as u64, (index * frame.len()) as u64),
        )
        .unwrap();
        host.tx
            .send(Event::Control(connection(3), consumed.encode().to_vec()))
            .await
            .unwrap();
    }
    // A later received frame proves the actor handled the last feedback while
    // its source's control writer remained full, without a timing sleep.
    host.tx
        .send(Event::Frame(connection(1), frame.clone()))
        .await
        .unwrap();
    assert_eq!(read(&mut receiver).await, frame);
    for _ in 0..controls {
        assert_eq!(&read(&mut host.sender).await[..4], b"WS2W");
    }
    assert!(host.sender.try_recv().is_err());
    host.tx.send(Event::Writable(connection(1))).await.unwrap();
    let grant = read(&mut host.sender).await;
    assert_eq!(&grant[..4], b"WS2W");
    assert_eq!(u32::from_be_bytes(grant[16..20].try_into().unwrap()), 1);
    // These tiny envelopes have not exhausted their startup byte allowance.
    assert_eq!(u32::from_be_bytes(grant[20..24].try_into().unwrap()), 0);
    assert!(host.sender.try_recv().is_err());
    drop(host);
}
