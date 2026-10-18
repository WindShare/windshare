use super::*;
use crate::{
    storage::Database,
    test_support::{Fixture, connection},
};
use anyhow::bail;
use std::{
    collections::VecDeque,
    sync::{Mutex, atomic::AtomicU64},
};
use tokio::time::timeout;
use windshare_relay_core::{
    challenge::ChallengeSeed, clock::UnixMillis, lifecycle::Record, share::Storage,
};

#[path = "actor_delivery_tests.rs"]
mod delivery;

#[derive(Clone)]
struct Frozen {
    now: Arc<AtomicU64>,
    seeds: Arc<Mutex<VecDeque<ChallengeSeed>>>,
}
impl Environment for Frozen {
    fn now(&self) -> UnixMillis {
        UnixMillis(self.now.load(Ordering::Acquire))
    }
    fn challenge(&self) -> Result<ChallengeSeed> {
        self.seeds
            .lock()
            .unwrap()
            .pop_front()
            .ok_or_else(|| anyhow!("injected entropy failure"))
    }
}

enum Receipt {
    Commit,
    FailedBeforeCommit,
    LostAfterCommit,
}
struct Attempt {
    operation: Storage,
    release: std::sync::mpsc::Sender<Receipt>,
}
#[derive(Clone)]
struct Controlled {
    database: Database,
    attempts: mpsc::UnboundedSender<Attempt>,
}
impl Persistence for Controlled {
    fn load(
        &self,
        share: windshare_relay_core::identifiers::ShareId,
    ) -> Result<Option<StoredAuthority>> {
        self.database.load(share)
    }
    fn execute(
        &self,
        share: windshare_relay_core::identifiers::ShareId,
        operation: &Storage,
    ) -> Result<Option<StoredAuthority>> {
        let (release, wait) = std::sync::mpsc::channel();
        self.attempts.send(Attempt {
            operation: operation.clone(),
            release,
        })?;
        match wait.recv()? {
            Receipt::Commit => self.database.execute(share, operation),
            Receipt::FailedBeforeCommit => bail!("write failed before commit"),
            Receipt::LostAfterCommit => {
                self.database.execute(share, operation)?;
                bail!("lost commit receipt");
            }
        }
    }
}

struct Harness {
    tx: mpsc::Sender<Event>,
    sender: crate::connection::Writer,
    stopper: crate::connection::Writer,
    attempts: mpsc::UnboundedReceiver<Attempt>,
    task: tokio::task::AbortHandle,
    generation: Generation,
    store: Database,
    fixture: Fixture,
    _directory: tempfile::TempDir,
}
impl Drop for Harness {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl Harness {
    async fn new() -> Self {
        let fixture = Fixture::new();
        let identity = fixture.identity();
        let directory = tempfile::tempdir().unwrap();
        let store =
            Database::open(&directory.path().join("authority.sqlite"), identity.relay).unwrap();
        let (attempt_tx, attempts) = mpsc::unbounded_channel();
        let controlled = Controlled {
            database: store.clone(),
            attempts: attempt_tx,
        };
        let environment = Frozen {
            now: Arc::new(AtomicU64::new(fixture.now(true).0)),
            seeds: Arc::new(Mutex::new(VecDeque::from([
                fixture.seed(true),
                fixture.seed(false),
            ]))),
        };
        let (tx, inbox) = mpsc::channel(32);
        let generation = Generation::new(identity, store.clone());
        let task = tokio::spawn(generation.track(Actor::start(
            controlled,
            environment.clone(),
            inbox,
            Arc::new(AtomicBool::new(false)),
            generation.clone(),
            Recovery::vacant(&store),
        )));
        let task = generation.supervise(task);
        let (stop_tx, mut stopper, _stop_closure) = crate::connection::channel(connection(2));
        tx.send(Event::Connected(connection(2), stop_tx))
            .await
            .unwrap();
        tx.send(Event::Control(connection(2), fixture.bytes("stopInitB64")))
            .await
            .unwrap();
        assert_eq!(&read(&mut stopper).await[..4], b"WS2Q");
        environment
            .now
            .store(fixture.now(false).0, Ordering::Release);
        let (sender_tx, mut sender, _sender_closure) = crate::connection::channel(connection(1));
        tx.send(Event::Connected(connection(1), sender_tx))
            .await
            .unwrap();
        tx.send(Event::Control(
            connection(1),
            fixture.bytes("registerInitB64"),
        ))
        .await
        .unwrap();
        assert_eq!(&read(&mut sender).await[..4], b"WS2Q");
        tx.send(Event::Control(
            connection(1),
            fixture.bytes("registerProofB64"),
        ))
        .await
        .unwrap();
        tx.send(Event::Control(
            connection(1),
            fixture.bytes("descriptorUploadB64"),
        ))
        .await
        .unwrap();
        Self {
            tx,
            sender,
            stopper,
            attempts,
            task,
            generation,
            store,
            fixture,
            _directory: directory,
        }
    }
    async fn attempt(&mut self) -> Attempt {
        timeout(Duration::from_secs(3), self.attempts.recv())
            .await
            .unwrap()
            .unwrap()
    }
    async fn stop(&self) {
        self.tx
            .send(Event::Control(
                connection(2),
                self.fixture.bytes("stopProofB64"),
            ))
            .await
            .unwrap();
    }
}

async fn read(rx: &mut crate::connection::Writer) -> Vec<u8> {
    timeout(Duration::from_secs(3), rx.recv())
        .await
        .unwrap()
        .unwrap()
}

#[tokio::test]
async fn acknowledgements_wait_for_atomic_registration_receipts_and_exact_retries() {
    for committed in [false, true] {
        let mut host = Harness::new().await;
        let ownership = host.attempt().await;
        assert!(matches!(ownership.operation, Storage::Register { .. }));
        assert!(
            host.store
                .load(host.fixture.identity().share)
                .unwrap()
                .is_none()
        );
        assert!(host.sender.try_recv().is_err());
        let expected_retry = ownership.operation.clone();
        ownership
            .release
            .send(if committed {
                Receipt::LostAfterCommit
            } else {
                Receipt::FailedBeforeCommit
            })
            .unwrap();

        let retry = host.attempt().await;
        assert_eq!(retry.operation, expected_retry);
        assert_eq!(
            matches!(
                host.store
                    .load(host.fixture.identity().share)
                    .unwrap()
                    .map(|stored| stored.record()),
                Some(Record::Published(_))
            ),
            committed
        );
        assert!(host.sender.try_recv().is_err());
        retry.release.send(Receipt::Commit).unwrap();
        assert_eq!(&read(&mut host.sender).await[..4], b"WS2K");

        host.stop().await;
        let stopping = host.attempt().await;
        assert!(host.stopper.try_recv().is_err());
        stopping.release.send(Receipt::Commit).unwrap();
        let stopped = host.attempt().await;
        assert!(matches!(
            host.store
                .load(host.fixture.identity().share)
                .unwrap()
                .map(|stored| stored.record()),
            Some(Record::Stopping(_))
        ));
        assert!(host.stopper.try_recv().is_err());
        stopped.release.send(Receipt::Commit).unwrap();
        assert_eq!(&read(&mut host.stopper).await[..4], b"WS2Y");
        assert!(matches!(
            host.store
                .load(host.fixture.identity().share)
                .unwrap()
                .map(|stored| stored.record()),
            Some(Record::Stopped(_))
        ));
        drop(host);
    }
}

#[tokio::test]
async fn startup_recovers_committed_registration_and_interrupted_stop_from_sqlite() {
    let fixture = Fixture::new();
    let identity = Identity {
        incarnation: crate::environment::incarnation().unwrap(),
        ..fixture.identity()
    };
    for committed in [1, 2] {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("authority.sqlite");
        let store = Database::open(&path, identity.relay).unwrap();
        for work in &fixture.works()[..committed] {
            store.execute(identity.share, work).unwrap();
        }
        drop(store);
        let store = Database::open(&path, identity.relay).unwrap();
        let idle = Arc::new(AtomicBool::new(false));
        let (_tx, inbox) = mpsc::channel(8);
        let environment = Frozen {
            now: Arc::new(AtomicU64::new(fixture.now(false).0)),
            seeds: Arc::new(Mutex::new(VecDeque::new())),
        };
        let generation = Generation::new(identity, store.clone());
        let task = tokio::spawn(generation.track(Actor::start(
            store.clone(),
            environment,
            inbox,
            Arc::clone(&idle),
            generation.clone(),
            Recovery::vacant(&store),
        )));
        timeout(Duration::from_secs(3), async {
            while !matches!(
                store
                    .load(identity.share)
                    .unwrap()
                    .map(|stored| stored.record()),
                Some(Record::Grace(_) | Record::Stopped(_))
            ) && !task.is_finished()
            {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        assert!(
            !task.is_finished(),
            "recovery task exited before reconciliation: {:?}",
            task.await
        );
        let record = store
            .load(identity.share)
            .unwrap()
            .map(|stored| stored.record());
        if committed == 1 {
            assert!(matches!(record, Some(Record::Grace(_))));
        } else {
            assert!(matches!(record, Some(Record::Stopped(_))));
        }
        task.abort();
        assert!(task.await.unwrap_err().is_cancelled());
    }
}

#[tokio::test]
async fn failed_actor_closes_mailbox_but_storage_still_fences_recovery() {
    let mut host = Harness::new().await;
    let publication = host.attempt().await;
    let (port, mut incoming, _) = crate::connection::channel(connection(3));
    host.tx
        .send(Event::Connected(connection(3), port))
        .await
        .unwrap();
    host.tx
        .send(Event::Control(
            connection(3),
            host.fixture.bytes("stopInitB64"),
        ))
        .await
        .unwrap();
    assert!(
        timeout(Duration::from_secs(3), incoming.recv())
            .await
            .unwrap()
            .is_none()
    );
    assert!(
        timeout(Duration::from_secs(3), host.sender.recv())
            .await
            .unwrap()
            .is_none()
    );
    host.tx.closed().await;
    assert!(host.generation.recovery().is_err());
    publication.release.send(Receipt::Commit).unwrap();
    timeout(Duration::from_secs(3), host.generation.wait_released())
        .await
        .unwrap();
    assert!(host.generation.recovery().is_ok());
    assert!(matches!(
        host.store
            .load(host.fixture.identity().share)
            .unwrap()
            .map(|stored| stored.record()),
        Some(Record::Published(_))
    ));
    drop(host);
}

#[tokio::test]
async fn cancelled_actor_cannot_lose_a_blocking_commit() {
    let mut host = Harness::new().await;
    let publication = host.attempt().await;
    host.task.abort();
    host.tx.closed().await;
    assert!(host.generation.recovery().is_err());
    publication.release.send(Receipt::LostAfterCommit).unwrap();
    timeout(Duration::from_secs(3), host.generation.wait_released())
        .await
        .unwrap();
    let identity = Identity {
        incarnation: crate::environment::incarnation().unwrap(),
        ..host.fixture.identity()
    };
    let generation = Generation::new(identity, host.store.clone());
    let (_tx, inbox) = mpsc::channel(8);
    let idle = Arc::new(AtomicBool::new(false));
    let task = tokio::spawn(generation.track(Actor::start(
        host.store.clone(),
        Frozen {
            now: Arc::new(AtomicU64::new(host.fixture.now(false).0)),
            seeds: Arc::new(Mutex::new(VecDeque::new())),
        },
        inbox,
        Arc::clone(&idle),
        generation.clone(),
        host.generation.recovery().unwrap(),
    )));
    let abort = generation.supervise(task);
    timeout(Duration::from_secs(3), async {
        while !matches!(
            host.store
                .load(identity.share)
                .unwrap()
                .map(|stored| stored.record()),
            Some(Record::Grace(_))
        ) {
            assert!(!abort.is_finished());
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    assert!(matches!(
        host.store
            .load(identity.share)
            .unwrap()
            .map(|stored| stored.record()),
        Some(Record::Grace(_))
    ));
    abort.abort();
    generation.wait_released().await;
    drop(host);
}

#[tokio::test]
async fn stop_supersedes_an_ownership_write_already_in_flight() {
    let mut host = Harness::new().await;
    let ownership = host.attempt().await;
    host.stop().await;
    // A same-mailbox marker proves STOP was processed while the write remains
    // blocked, without relying on a scheduling sleep or pretending it failed.
    host.tx
        .send(Event::Closed(connection(1), oneshot::channel().0))
        .await
        .unwrap();
    assert!(
        timeout(Duration::from_secs(3), host.sender.recv())
            .await
            .unwrap()
            .is_none()
    );
    ownership.release.send(Receipt::Commit).unwrap();
    host.attempt().await.release.send(Receipt::Commit).unwrap();
    host.attempt().await.release.send(Receipt::Commit).unwrap();
    assert_eq!(&read(&mut host.stopper).await[..4], b"WS2Y");
    assert!(matches!(
        host.store
            .load(host.fixture.identity().share)
            .unwrap()
            .map(|stored| stored.record()),
        Some(Record::Stopped(_))
    ));
    drop(host);
}
