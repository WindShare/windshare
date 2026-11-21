use super::*;
use crate::connection::Writer;
use windshare_relay_core::{
    descriptor_upload::DescriptorUpload,
    flow::{Counts, DeliveryConsumed, SettlementAck, SettlementNotice},
    identifiers::RelaySessionId,
    sessions::MAX_OPAQUE_CIPHERTEXT_BYTES,
};

async fn published() -> Harness {
    let mut host = Harness::new().await;
    host.attempt().await.release.send(Receipt::Commit).unwrap();
    assert_eq!(&read(&mut host.sender).await[..4], b"WS2K");
    host
}

async fn join(host: &Harness, id: u64) -> (Writer, RelaySessionId) {
    let (port, mut receiver, _) =
        crate::connection::channel(connection(id), crate::node::Runtime::default());
    host.tx
        .send(Event::Connected(connection(id), Box::new(port)))
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
    (receiver, session)
}

async fn joined(host: &mut Harness, id: u64) -> (Writer, RelaySessionId) {
    // The vectors declare 16 frames. Toward the shared sender, ToSender uses
    // the bounded TO_SENDER_DELIVERY_WINDOW (1 startup + 7 ordinary = 8 frames).
    // Toward the receiver, 14 ordinary plus startup leave one protected slot.
    let (mut receiver, session) = join(host, id).await;
    for (writer, granted) in [
        (
            &mut receiver,
            crate::config::TO_SENDER_DELIVERY_WINDOW.frames,
        ),
        (&mut host.sender, 15),
    ] {
        let mut frames = 0;
        for _ in 0..2 {
            let grant = read(writer).await;
            assert_eq!(&grant[..4], b"WS2W");
            frames += u32::from_be_bytes(grant[16..20].try_into().unwrap());
        }
        assert_eq!(u64::from(frames), granted);
    }
    (receiver, session)
}

fn opaque(session: RelaySessionId) -> Vec<u8> {
    opaque_with_ciphertext_len(session, 1)
}

fn opaque_with_ciphertext_len(session: RelaySessionId, length: u32) -> Vec<u8> {
    let mut bytes = b"WS2O\x02\0\0\0".to_vec();
    bytes.extend_from_slice(session.as_bytes());
    bytes.extend_from_slice(&length.to_be_bytes());
    bytes.resize(bytes.len() + length as usize, 7);
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
    let (intruder, mut refused, _) =
        crate::connection::channel(connection(4), crate::node::Runtime::default());
    host.tx
        .send(Event::Connected(connection(4), Box::new(intruder)))
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
    let (port, mut healthy, _) =
        crate::connection::channel(connection(4), crate::node::Runtime::default());
    host.tx
        .send(Event::Connected(connection(4), Box::new(port)))
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
async fn full_capacity_notice_budget_preserves_settlement_and_sibling_delivery() {
    use crate::config::{CAPACITY_NOTICE_QUEUE_FRAMES, SUBMITTED_DATA_FRAMES};

    let mut host = published().await;
    let (mut healthy, healthy_session) = joined(&mut host, 3).await;
    let mut slow = Vec::new();
    for index in 0..=CAPACITY_NOTICE_QUEUE_FRAMES {
        let id = 4 + index as u64;
        let (receiver, session) = join(&host, id).await;
        let request = opaque_with_ciphertext_len(session, MAX_OPAQUE_CIPHERTEXT_BYTES);
        host.tx
            .send(Event::Frame(connection(id), request.clone()))
            .await
            .unwrap();
        loop {
            let frame = read(&mut host.sender).await;
            if frame == request {
                break;
            }
            assert!(frame.starts_with(b"WS2W"));
        }
        // A full-sized request returns both the frame and byte startup allowance
        // after consumption, leaving room for the next receiver to join.
        let consumed =
            DeliveryConsumed::new(session, Counts::new(1, request.len() as u64)).unwrap();
        host.tx
            .send(Event::Control(connection(1), consumed.encode().to_vec()))
            .await
            .unwrap();
        slow.push((connection(id), receiver, session));
    }

    tokio::time::pause();
    // Each slow writer keeps one accepted frame waiting behind its submitted prefix.
    for (_, _, session) in &slow {
        for _ in 0..=SUBMITTED_DATA_FRAMES {
            host.tx
                .send(Event::Frame(connection(1), opaque(*session)))
                .await
                .unwrap();
        }
    }
    let healthy_frame = opaque(healthy_session);
    host.tx
        .send(Event::Frame(connection(1), healthy_frame.clone()))
        .await
        .unwrap();
    assert_eq!(read(&mut healthy).await, healthy_frame);
    tokio::time::advance(Duration::from_secs(2)).await;

    // Observe the reporting tick at every receiver while the sender stays blocked.
    for (_, receiver, session) in &mut slow {
        loop {
            let frame = read(receiver).await;
            if frame.starts_with(b"WS2Z") {
                assert_eq!(&frame[8..16], session.as_bytes());
                break;
            }
            assert!(frame.starts_with(b"WS2W") || frame == opaque(*session));
        }
    }

    let (retired_connection, _, retired_session) = &slow[0];
    host.tx
        .send(Event::Disconnected(*retired_connection))
        .await
        .unwrap();
    host.tx
        .send(Event::Frame(connection(1), healthy_frame.clone()))
        .await
        .unwrap();

    // The actor must process the disconnect before this payload, with no writer
    // release at the shared sender to make room for the required settlement.
    assert_eq!(read(&mut healthy).await, healthy_frame);
    for _ in 0..CAPACITY_NOTICE_QUEUE_FRAMES {
        assert!(read(&mut host.sender).await.starts_with(b"WS2Z"));
    }
    let notice = SettlementNotice::decode(&read(&mut host.sender).await).unwrap();
    assert_eq!(notice.session(), *retired_session);
    assert!(host.sender.try_recv().is_err());
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

const DEFAULT_CAPACITY_FRAMES: u64 = 256;
const DEFAULT_CAPACITY_BYTES: u64 = 16 << 20;

struct SignedShare {
    identity: Identity,
    key: ed25519_dalek::SigningKey,
    share_id: [u8; 12],
    share_instance: [u8; 16],
    pk_hash: [u8; 16],
    descriptor_digest: [u8; 32],
    resume_token_hash: [u8; 32],
    relay_bytes: [u8; 32],
    register_init: Vec<u8>,
    descriptor_upload: Vec<u8>,
    join_frame: Vec<u8>,
}

impl SignedShare {
    fn new() -> Self {
        use ed25519_dalek::SigningKey;
        use sha2::{Digest, Sha256};
        use windshare_relay_core::{
            identifiers::{RelayIdentity, ShareId},
            lifecycle::Incarnation,
        };

        let key = SigningKey::from_bytes(&[7; 32]);
        let pubkey = key.verifying_key().to_bytes();
        let pk_hash: [u8; 16] = Sha256::new()
            .chain_update(b"windshare/v2 sender-key\0")
            .chain_update(pubkey)
            .finalize()[..16]
            .try_into()
            .unwrap();
        let share_id: [u8; 12] = Sha256::new()
            .chain_update(b"windshare/v2 share-id\0")
            .chain_update(pk_hash)
            .finalize()[..12]
            .try_into()
            .unwrap();
        let share_instance = [0x11_u8; 16];
        let descriptor = b"sealed-default-capacity-descriptor";
        let descriptor_digest: [u8; 32] = Sha256::digest(descriptor).into();
        let resume_token_hash = [0x22_u8; 32];
        let relay_bytes = [0x33_u8; 32];
        let identity = Identity {
            relay: RelayIdentity::new(relay_bytes).unwrap(),
            share: ShareId::new(share_id).unwrap(),
            incarnation: Incarnation::new([9; 16]).unwrap(),
        };

        let mut register_init = Vec::with_capacity(132);
        register_init.extend_from_slice(b"WS2R\x02\x00\x00\x00");
        register_init.extend_from_slice(&share_id);
        register_init.extend_from_slice(&share_instance);
        register_init.extend_from_slice(&pk_hash);
        register_init.extend_from_slice(&descriptor_digest);
        register_init.extend_from_slice(&resume_token_hash);
        register_init.extend_from_slice(&DEFAULT_CAPACITY_FRAMES.to_be_bytes());
        register_init.extend_from_slice(&DEFAULT_CAPACITY_BYTES.to_be_bytes());

        let mut descriptor_upload = Vec::with_capacity(12 + descriptor.len());
        descriptor_upload.extend_from_slice(b"WS2U\x02\x00\x00\x00");
        descriptor_upload
            .extend_from_slice(&u32::try_from(descriptor.len()).unwrap().to_be_bytes());
        descriptor_upload.extend_from_slice(descriptor);

        let mut join_frame = Vec::with_capacity(36);
        join_frame.extend_from_slice(b"WS2J\x02\x00\x00\x00");
        join_frame.extend_from_slice(&share_id);
        join_frame.extend_from_slice(&DEFAULT_CAPACITY_FRAMES.to_be_bytes());
        join_frame.extend_from_slice(&DEFAULT_CAPACITY_BYTES.to_be_bytes());

        Self {
            identity,
            key,
            share_id,
            share_instance,
            pk_hash,
            descriptor_digest,
            resume_token_hash,
            relay_bytes,
            register_init,
            descriptor_upload,
            join_frame,
        }
    }

    fn sign_challenge(&self, challenge: &[u8]) -> Vec<u8> {
        use ed25519_dalek::Signer;

        let mut preimage = Vec::new();
        preimage.extend_from_slice(b"windshare/v2 relay-register\0");
        preimage.extend_from_slice(&self.share_id);
        preimage.extend_from_slice(&self.share_instance);
        preimage.extend_from_slice(&self.pk_hash);
        preimage.extend_from_slice(&self.descriptor_digest);
        preimage.extend_from_slice(&self.resume_token_hash);
        preimage.extend_from_slice(&self.relay_bytes);
        preimage.extend_from_slice(&challenge[8..64]);
        preimage.extend_from_slice(&DEFAULT_CAPACITY_FRAMES.to_be_bytes());
        preimage.extend_from_slice(&DEFAULT_CAPACITY_BYTES.to_be_bytes());
        let signature = self.key.sign(&preimage).to_bytes();

        let mut proof = Vec::with_capacity(104);
        proof.extend_from_slice(b"WS2P\x02\x00\x00\x00");
        proof.extend_from_slice(&self.key.verifying_key().to_bytes());
        proof.extend_from_slice(&signature);
        proof
    }
}

struct DefaultCapacityShare {
    tx: mpsc::Sender<Event>,
    sender: Writer,
    join_frame: Vec<u8>,
    _attempts: mpsc::UnboundedReceiver<Attempt>,
    task: tokio::task::AbortHandle,
    _directory: tempfile::TempDir,
}

impl Drop for DefaultCapacityShare {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl DefaultCapacityShare {
    async fn start() -> Self {
        let signed = SignedShare::new();
        let directory = tempfile::tempdir().unwrap();
        let store = Database::open(
            &directory.path().join("authority.sqlite"),
            signed.identity.relay,
        )
        .unwrap();
        let (attempt_tx, mut attempts) = mpsc::unbounded_channel();
        let controlled = Controlled {
            database: store.clone(),
            attempts: attempt_tx,
        };
        let environment = Frozen {
            now: Arc::new(AtomicU64::new(1_700_000_000_000)),
            seeds: Arc::new(Mutex::new(VecDeque::from([ChallengeSeed::new(
                [0x44; 16], [0x55; 32],
            )
            .unwrap()]))),
        };
        let (tx, inbox) = mpsc::channel(64);
        let generation = Generation::new(signed.identity, store.clone());
        let task = tokio::spawn(generation.track(Actor::start(
            controlled,
            environment,
            inbox,
            Arc::new(AtomicBool::new(false)),
            generation.clone(),
            Recovery::vacant(&store),
            Runtime::default(),
        )));
        let task = generation.supervise(task);

        let (sender_port, mut sender, _) =
            crate::connection::channel(connection(1), Runtime::default());
        tx.send(Event::Connected(connection(1), Box::new(sender_port)))
            .await
            .unwrap();
        tx.send(Event::Control(connection(1), signed.register_init.clone()))
            .await
            .unwrap();
        let challenge = read(&mut sender).await;
        assert_eq!(&challenge[..4], b"WS2Q");
        tx.send(Event::Control(
            connection(1),
            signed.sign_challenge(&challenge),
        ))
        .await
        .unwrap();
        tx.send(Event::Control(
            connection(1),
            signed.descriptor_upload.clone(),
        ))
        .await
        .unwrap();

        let attempt = timeout(Duration::from_secs(3), attempts.recv())
            .await
            .unwrap()
            .unwrap();
        attempt.release.send(Receipt::Commit).unwrap();
        assert_eq!(&read(&mut sender).await[..4], b"WS2K");

        Self {
            tx,
            sender,
            join_frame: signed.join_frame,
            _attempts: attempts,
            task,
            _directory: directory,
        }
    }

    async fn join(&self, id: u64) -> (Writer, RelaySessionId) {
        let (port, mut receiver, _) =
            crate::connection::channel(connection(id), Runtime::default());
        self.tx
            .send(Event::Connected(connection(id), Box::new(port)))
            .await
            .unwrap();
        self.tx
            .send(Event::Control(connection(id), self.join_frame.clone()))
            .await
            .unwrap();
        let descriptor = read(&mut receiver).await;
        assert_eq!(&descriptor[..4], b"WS2D");
        let session = RelaySessionId::new(descriptor[8..16].try_into().unwrap()).unwrap();
        (receiver, session)
    }

    async fn join_and_idle(&mut self, id: u64) -> (u64, Writer, RelaySessionId) {
        let target = crate::config::TO_SENDER_DELIVERY_WINDOW;
        let (mut receiver, session) = self.join(id).await;
        let first = read_grant(&mut receiver).await;
        let second = read_grant(&mut receiver).await;
        assert_eq!(first.frames + second.frames, target.frames, "receiver {id}");
        assert_eq!(
            first.encoded_bytes + second.encoded_bytes,
            target.encoded_bytes,
            "receiver {id}"
        );
        // Consume one small envelope so early receivers pin startup bytes,
        // then refill and idle with the full window held.
        let request = opaque(session);
        self.tx
            .send(Event::Frame(connection(id), request.clone()))
            .await
            .unwrap();
        loop {
            let frame = read(&mut self.sender).await;
            if frame == request {
                break;
            }
            assert!(frame.starts_with(b"WS2W"));
        }
        let consumed =
            DeliveryConsumed::new(session, Counts::new(1, request.len() as u64)).unwrap();
        self.tx
            .send(Event::Control(connection(1), consumed.encode().to_vec()))
            .await
            .unwrap();
        let refill = read_grant(&mut receiver).await;
        assert_eq!(refill, Counts::new(1, request.len() as u64));
        (id, receiver, session)
    }
}

async fn read_grant(writer: &mut Writer) -> Counts {
    let grant = read(writer).await;
    assert_eq!(&grant[..4], b"WS2W");
    Counts::new(
        u64::from(u32::from_be_bytes(grant[16..20].try_into().unwrap())),
        u64::from(u32::from_be_bytes(grant[20..24].try_into().unwrap())),
    )
}

#[tokio::test]
async fn all_business_receivers_get_equal_sender_windows_and_stay_isolated() {
    // Every business receiver (up to 31) must join and hold the full
    // TO_SENDER_DELIVERY_WINDOW regardless of join order while earlier siblings
    // idle, and active consumption must refill only the active flow without
    // leaking grants to idle siblings.
    const RECEIVERS: u64 = 31;
    let mut share = DefaultCapacityShare::start().await;
    let mut receivers = Vec::new();
    for id in 3..3 + RECEIVERS {
        receivers.push(share.join_and_idle(id).await);
    }

    let active_id = receivers[0].0;
    let active_session = receivers[0].2;
    let active_request = opaque(active_session);
    for seq in 2_u64..=11 {
        share
            .tx
            .send(Event::Frame(connection(active_id), active_request.clone()))
            .await
            .unwrap();
        assert_eq!(read(&mut share.sender).await, active_request);
        let consumed = DeliveryConsumed::new(
            active_session,
            Counts::new(seq, seq * active_request.len() as u64),
        )
        .unwrap();
        share
            .tx
            .send(Event::Control(connection(1), consumed.encode().to_vec()))
            .await
            .unwrap();
        let refill = read_grant(&mut receivers[0].1).await;
        assert_eq!(refill, Counts::new(1, active_request.len() as u64));
    }
    for (id, receiver, _) in receivers.iter_mut().skip(1) {
        assert!(
            receiver.try_recv().is_err(),
            "idle sibling {id} received unsolicited credit"
        );
    }
    drop(receivers);
    drop(share);
}

#[tokio::test]
async fn settling_receivers_do_not_block_replacement_admissions_on_shared_sender() {
    const RECEIVERS: u64 = 31;
    const CHURN: usize = 3;
    let target = crate::config::TO_SENDER_DELIVERY_WINDOW;
    let mut share = DefaultCapacityShare::start().await;
    let mut receivers = Vec::new();
    for id in 3..3 + RECEIVERS {
        receivers.push(share.join_and_idle(id).await);
    }

    // Light settling churn: disconnect 3 receivers with one unacknowledged
    // submitted frame each (3 frames total, within the 6 ordinary + 2 startup
    // frame headroom left at 31 * 8 = 248 / 256) and close their sockets to
    // free business connection slots without sending WS2S.
    for _ in 0..CHURN {
        let (id, receiver, session) = receivers.pop().unwrap();
        let pending = opaque(session);
        share
            .tx
            .send(Event::Frame(connection(id), pending.clone()))
            .await
            .unwrap();
        assert_eq!(read(&mut share.sender).await, pending);
        share
            .tx
            .send(Event::Disconnected(connection(id)))
            .await
            .unwrap();
        drop(receiver);
        let (closed_tx, closed_rx) = oneshot::channel();
        share
            .tx
            .send(Event::Closed(connection(id), closed_tx))
            .await
            .unwrap();
        closed_rx.await.unwrap();
        let notice = SettlementNotice::decode(&read(&mut share.sender).await).unwrap();
        assert_eq!(notice.session(), session);
    }
    for offset in 0..CHURN as u64 {
        let id = 100 + offset;
        let (mut receiver, session) = share.join(id).await;
        let first = read_grant(&mut receiver).await;
        let second = read_grant(&mut receiver).await;
        assert_eq!(
            first.frames + second.frames,
            target.frames,
            "replacement {id}"
        );
        assert_eq!(
            first.encoded_bytes + second.encoded_bytes,
            target.encoded_bytes,
            "replacement {id}"
        );
        for _ in 0..2 {
            assert_eq!(&read(&mut share.sender).await[..4], b"WS2W");
        }
        receivers.push((id, receiver, session));
    }
    drop(receivers);
    drop(share);
}
