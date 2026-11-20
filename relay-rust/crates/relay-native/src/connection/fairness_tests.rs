use super::*;
use crate::node::access::{Policy, TrustedIssuer};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ed25519_dalek::SigningKey;
use windshare_relay_access::{
    Credential, CredentialState, Digest,
    entitlement::{DAY_SECONDS, ENTITLEMENT_LIFETIME, Entitlement},
    trust::Generation,
};

const FRAME: usize = 2_048;
const HEADER_BYTES: usize = OpaqueHeader::MIN_ENCODED_BYTES as usize - 1;
const BACKLOG: usize = 4;
const OWNER: Attribution = Attribution::Credential {
    principal: [7; 16],
    credential: [7; 16],
};

struct Stream {
    scenario: Scenario,
    outbox: Outbox,
    writer: Writer,
    wake: Arc<Notify>,
    attribution: Attribution,
    sent: usize,
}

impl Stream {
    fn new(
        node: &Runtime,
        incarnation: u8,
        attribution: Attribution,
        size: usize,
        count: usize,
    ) -> Self {
        let mut scenario = Scenario::with_incarnation(incarnation);
        let (mut outbox, writer, _) = channel(connection(3), node.clone());
        let wake = Arc::new(Notify::new());
        outbox.set_wake(Arc::clone(&wake));
        if count > 1 {
            scenario.grant(count as u64 - 1);
        }
        for _ in 0..count {
            let (receipt, bytes) = scenario.receive(size - HEADER_BYTES);
            outbox.queue(receipt, bytes, attribution).unwrap();
        }
        Self {
            scenario,
            outbox,
            writer,
            wake,
            attribution,
            sent: 0,
        }
    }

    fn submit(&mut self) -> bool {
        let now = self.scenario.fixture.now(false);
        let Some(step) = self
            .outbox
            .submit_next(&mut self.scenario.relay, now, self.attribution)
        else {
            return false;
        };
        assert_eq!(step.refusal, None);
        true
    }

    fn serve_backlog(&mut self) {
        while self.submit() {
            let size = self.writer.try_recv().unwrap().len();
            self.sent += size;
            let now = self.scenario.fixture.now(false);
            let consumed = Input::Consumed {
                connection: self.scenario.flow.destination(),
                session: self.scenario.session.wire_id(),
                total: Counts::new((self.sent / size) as u64, self.sent as u64),
            };
            assert_eq!(self.scenario.relay.handle(consumed, now).refusal, None);
            let credit = Counts::new(1, size as u64);
            assert_eq!(
                self.scenario
                    .relay
                    .handle(
                        Input::Grant {
                            flow: self.scenario.flow,
                            credit
                        },
                        now
                    )
                    .refusal,
                None
            );
            let (receipt, bytes) = self.scenario.receive(size - HEADER_BYTES);
            self.outbox.queue(receipt, bytes, self.attribution).unwrap();
        }
    }

    fn drain(&mut self) {
        while self.submit() {
            self.sent += self.writer.try_recv().unwrap().len();
        }
    }
}

fn network(rate: u64) -> (tempfile::TempDir, Runtime, [Attribution; 2]) {
    let root = SigningKey::from_bytes(&[1; 32]);
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let generation = Generation {
        issuer: root.verifying_key().to_bytes(),
        key: SigningKey::from_bytes(&[2; 32]).verifying_key().to_bytes(),
        number: 1,
        not_before: now - 1,
        expires_at: now + DAY_SECONDS,
    };
    let settings = Settings {
        capacity_bytes_per_second: Some(rate),
        access: Policy {
            trusted_issuers: vec![TrustedIssuer {
                root: URL_SAFE_NO_PAD.encode(generation.issuer),
                certificate: URL_SAFE_NO_PAD.encode(generation.certify(&root).unwrap()),
            }],
            owner_principals: vec![[7; 16]],
            credentials: vec![Credential {
                principal: [7; 16],
                id: [7; 16],
                name: "owner".into(),
                digest: Digest::of(b"owner"),
                created_at: now,
                last_used_at: None,
                state: CredentialState::Active,
            }],
            ..Policy::default()
        },
        ..Settings::default()
    };
    let directory = tempfile::tempdir().unwrap();
    let database = Database::open(
        &directory.path().join("node.sqlite"),
        Fixture::new().identity().relay,
    )
    .unwrap();
    let node = Runtime::new(settings, database).unwrap();
    let accounts = [1, 2].map(|id| {
        let account = [id; 16];
        assert!(node.access.refresh(
            generation.issuer,
            account,
            Entitlement {
                version: 1,
                day: now / DAY_SECONDS,
                valid_until: now + ENTITLEMENT_LIFETIME,
                remaining_bytes: 1 << 30,
            },
            now
        ));
        Attribution::Account {
            issuer: generation.issuer,
            account,
            credential: account,
        }
    });
    // Spend startup credit so all competing heads are visible before each two-frame burst.
    let mut warmup = Stream::new(
        &node,
        99,
        OWNER,
        OpaqueHeader::MAX_ENCODED_BYTES as usize,
        1,
    );
    warmup.drain();
    assert_eq!(warmup.sent, OpaqueHeader::MAX_ENCODED_BYTES as usize);
    (directory, node, accounts)
}

#[tokio::test(start_paused = true)]
async fn writable_successors_keep_public_byte_shares_across_two_frame_bursts() {
    let (_dir, node, [account, _]) = network(2 * FRAME as u64);
    let mut standard = Stream::new(&node, 1, account, FRAME, BACKLOG);
    let mut lowest = Stream::new(&node, 2, Attribution::Anonymous, FRAME, BACKLOG);
    standard.serve_backlog();
    lowest.serve_backlog();
    for _ in 0..20 {
        tokio::time::advance(Duration::from_secs(1)).await;
        lowest.serve_backlog();
        standard.serve_backlog();
    }
    assert_eq!((standard.sent, lowest.sent), (38 * FRAME, 2 * FRAME));
}

#[tokio::test(start_paused = true)]
async fn eight_sessions_share_the_same_account_budget_as_one_session() {
    let (_dir, node, [one, many]) = network(2 * FRAME as u64);
    let mut streams: Vec<_> = (1..=9)
        .map(|id| Stream::new(&node, id, if id == 1 { one } else { many }, FRAME, BACKLOG))
        .collect();
    for stream in &mut streams {
        stream.serve_backlog();
    }
    for _ in 0..24 {
        tokio::time::advance(Duration::from_secs(1)).await;
        for stream in &mut streams {
            stream.serve_backlog();
        }
    }
    assert_eq!(streams[0].sent, 24 * FRAME);
    assert!(streams[1..].iter().all(|stream| stream.sent == 3 * FRAME));
}

#[tokio::test(start_paused = true)]
async fn a_full_owner_writer_yields_until_writable_and_small_signaling_can_go_first() {
    let bulk = 64 << 10;
    let (_dir, node, [account, _]) = network(bulk as u64);
    let mut owner = Stream::new(&node, 1, OWNER, FRAME, 4);
    let mut standard = Stream::new(&node, 2, account, bulk, 4);
    let mut signal = Stream::new(&node, 3, Attribution::Anonymous, FRAME, 2);
    for stream in [&mut standard, &mut signal, &mut owner] {
        assert!(!stream.submit());
    }
    tokio::time::advance(Duration::from_millis(63)).await;
    assert!(!signal.submit());
    for _ in 0..SUBMITTED_DATA_FRAMES {
        assert!(owner.submit());
    }
    assert!(!owner.submit());
    tokio::time::advance(Duration::from_secs(1)).await;
    assert!(!standard.submit());
    assert!(signal.submit());
    assert_eq!(signal.writer.try_recv().unwrap().len(), FRAME);
    assert_eq!(owner.writer.try_recv().unwrap().len(), FRAME);
    assert!(owner.submit());
    tokio::time::advance(Duration::from_secs(1)).await;
    standard.drain();
    assert!(standard.sent >= bulk);
    // The stalled sockets must still own their slots while the public writer drains.
    drop((owner, standard, signal));
}
#[test]
fn competing_flows_share_one_writer_window_and_discard_releases_its_reserved_slot() {
    let node = Runtime::default();
    let window = Arc::default();
    let wake = Arc::default();
    let register = |id| {
        node.governor
            .register(
                Scenario::with_incarnation(id).flow,
                Attribution::Anonymous,
                Arc::clone(&wake),
                Arc::clone(&window),
            )
            .unwrap()
    };
    let first = register(1);
    let second = register(2);
    let third = register(3);
    let sizes = [100, 500];
    let offers = [&first, &second, &third]
        .into_iter()
        .map(|lane| (lane, sizes.into_iter()));
    let (index, first_permit) = node.governor.take(&window, offers).unwrap();
    assert_eq!(index, 0);
    // One frame is in the writer, one is promised to the second lane.
    assert_eq!(window.frames.available_permits(), 1);
    assert_eq!(
        window.bytes.available_permits(),
        crate::config::SUBMITTED_DATA_BYTES - 100
    );
    drop(second);
    assert!(wake.notified().now_or_never().is_some());
    assert_eq!(window.frames.available_permits(), 1);
    let remaining = [(&first, [500].as_slice()), (&third, sizes.as_slice())];
    let (index, third_permit) = node
        .governor
        .take(
            &window,
            remaining
                .into_iter()
                .map(|(lane, head)| (lane, head.iter().copied())),
        )
        .unwrap();
    assert_eq!(index, 1);
    assert_eq!(window.frames.available_permits(), 0);
    drop((first_permit, third_permit));
    let (index, successor) = node
        .governor
        .take(
            &window,
            [&first, &third]
                .into_iter()
                .map(|lane| (lane, std::iter::once(500))),
        )
        .unwrap();
    assert_eq!(index, 0);
    assert_eq!(successor.bytes.num_permits(), 500);
}

#[path = "burst_tests.rs"]
mod bursts;
