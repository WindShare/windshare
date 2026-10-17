use super::*;
use crate::{
    storage::Database,
    test_support::{Fixture, connection},
};
use std::sync::{Mutex, atomic::AtomicU64};
use tokio::time::timeout;
use windshare_relay_core::{
    challenge::ChallengeSeed, clock::UnixMillis, lifecycle::Record, share::Storage,
};

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
        Ok(self.seeds.lock().unwrap().pop_front().unwrap())
    }
}

enum Receipt {
    Commit,
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
    fn load(&self, share: windshare_relay_core::identifiers::ShareId) -> Result<Option<Record>> {
        self.database.load(share)
    }
    fn execute(
        &self,
        share: windshare_relay_core::identifiers::ShareId,
        operation: &Storage,
    ) -> Result<()> {
        let (release, wait) = std::sync::mpsc::channel();
        self.attempts.send(Attempt {
            operation: operation.clone(),
            release,
        })?;
        let receipt = wait.recv()?;
        self.database.execute(share, operation)?;
        match receipt {
            Receipt::Commit => Ok(()),
            Receipt::LostAfterCommit => bail!("lost commit receipt"),
        }
    }
}

struct Harness {
    tx: mpsc::Sender<Event>,
    sender: mpsc::Receiver<Vec<u8>>,
    stopper: mpsc::Receiver<Vec<u8>>,
    attempts: mpsc::UnboundedReceiver<Attempt>,
    task: JoinHandle<Result<()>>,
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
        let task = tokio::spawn(Actor::start(
            identity,
            controlled,
            environment.clone(),
            inbox,
            Arc::new(AtomicBool::new(false)),
        ));
        let (stop_tx, mut stopper) = mpsc::channel(8);
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
        let (sender_tx, mut sender) = mpsc::channel(8);
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

async fn read(rx: &mut mpsc::Receiver<Vec<u8>>) -> Vec<u8> {
    timeout(Duration::from_secs(3), rx.recv())
        .await
        .unwrap()
        .unwrap()
}

#[tokio::test]
async fn acknowledgements_wait_for_durable_receipts_and_uncertain_cas_retries() {
    let mut host = Harness::new().await;
    let publication = host.attempt().await;
    assert!(matches!(publication.operation, Storage::Publish { .. }));
    assert!(host.sender.try_recv().is_err());
    publication.release.send(Receipt::Commit).unwrap();

    let ownership = host.attempt().await;
    assert!(matches!(
        host.store.load(host.fixture.identity().share).unwrap(),
        Some(Record::Unresolved(_))
    ));
    assert!(host.sender.try_recv().is_err());
    let expected_retry = ownership.operation.clone();
    ownership.release.send(Receipt::LostAfterCommit).unwrap();

    let retry = host.attempt().await;
    assert_eq!(retry.operation, expected_retry);
    assert!(matches!(
        host.store.load(host.fixture.identity().share).unwrap(),
        Some(Record::Published(_))
    ));
    assert!(host.sender.try_recv().is_err());
    retry.release.send(Receipt::Commit).unwrap();
    assert_eq!(&read(&mut host.sender).await[..4], b"WS2K");

    host.stop().await;
    let stopping = host.attempt().await;
    assert!(host.stopper.try_recv().is_err());
    stopping.release.send(Receipt::Commit).unwrap();
    let stopped = host.attempt().await;
    assert!(matches!(
        host.store.load(host.fixture.identity().share).unwrap(),
        Some(Record::Stopping(_))
    ));
    assert!(host.stopper.try_recv().is_err());
    stopped.release.send(Receipt::Commit).unwrap();
    assert_eq!(&read(&mut host.stopper).await[..4], b"WS2Y");
    assert!(matches!(
        host.store.load(host.fixture.identity().share).unwrap(),
        Some(Record::Stopped(_))
    ));
}

#[tokio::test]
async fn startup_reconciles_interrupted_publication_and_stop_from_sqlite() {
    let fixture = Fixture::new();
    let identity = Identity {
        incarnation: crate::environment::incarnation().unwrap(),
        ..fixture.identity()
    };
    for committed in [1, 3] {
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
        let task = tokio::spawn(Actor::start(
            identity,
            store.clone(),
            environment,
            inbox,
            Arc::clone(&idle),
        ));
        timeout(Duration::from_secs(3), async {
            while !idle.load(Ordering::Acquire) && !task.is_finished() {
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
        assert!(idle.load(Ordering::Acquire));
        let record = store.load(identity.share).unwrap();
        if committed == 1 {
            assert!(record.is_none());
        } else {
            assert!(matches!(record, Some(Record::Stopped(_))));
        }
        task.abort();
        assert!(task.await.unwrap_err().is_cancelled());
    }
}

#[tokio::test]
async fn stop_supersedes_an_ownership_write_already_in_flight() {
    let mut host = Harness::new().await;
    host.attempt().await.release.send(Receipt::Commit).unwrap();
    let ownership = host.attempt().await;
    host.stop().await;
    // A same-mailbox marker proves STOP was processed while the write remains
    // blocked, without relying on a scheduling sleep or pretending it failed.
    host.tx.send(Event::Closed(connection(1))).await.unwrap();
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
        host.store.load(host.fixture.identity().share).unwrap(),
        Some(Record::Stopped(_))
    ));
}
