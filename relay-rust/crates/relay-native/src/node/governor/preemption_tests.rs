use super::*;
use std::{io, sync::Arc};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    task::JoinSet,
    time::{self, MissedTickBehavior},
};

const FRAME: usize = 4096;
const SLOT: Duration = Duration::from_millis(8);
const LINK: u64 = 512_000;

async fn socket_pair() -> (TcpStream, TcpStream) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let (sender, receiver) = tokio::join!(
        TcpStream::connect(listener.local_addr().unwrap()),
        listener.accept()
    );
    let sender = sender.unwrap();
    let receiver = receiver.unwrap().0;
    sender.set_nodelay(true).unwrap();
    receiver.set_nodelay(true).unwrap();
    (sender, receiver)
}

// Both real TCP streams share one frame-sized service opportunity per tick.
// End-to-end acknowledgments bound kernel buffering so a completed write is
// attributable to the emulated uplink even on hosts with large loopback buffers.
async fn bottleneck(mut sockets: [TcpStream; 2]) -> io::Result<()> {
    let mut cursor = 0;
    let mut received = [0; 2];
    let mut buf = [0; FRAME];
    let mut slots = time::interval(SLOT);
    slots.set_missed_tick_behavior(MissedTickBehavior::Skip);
    loop {
        slots.tick().await;
        for i in [cursor, 1 - cursor] {
            match sockets[i].try_read(&mut buf[..FRAME - received[i]]) {
                Ok(0) => return Ok(()),
                Ok(n) => {
                    received[i] += n;
                    if received[i] == FRAME {
                        sockets[i].write_all(&[1]).await?;
                        received[i] = 0;
                    }
                    cursor = 1 - i;
                    break;
                }
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {}
                Err(e) => return Err(e),
            }
        }
    }
}

async fn send(
    mut socket: TcpStream,
    owner: bool,
    limit: Arc<AtomicU64>,
    wire: Arc<Egress>,
    total: Arc<AtomicU64>,
) -> io::Result<()> {
    let packet = [0x57; FRAME];
    let mut earliest = Instant::now();
    loop {
        let rate = limit.load(Ordering::Relaxed);
        if rate == 0 {
            time::sleep(SLOT).await;
            continue;
        }
        earliest = earliest.max(Instant::now() - SLOT)
            + Duration::from_nanos(FRAME as u64 * 1_000_000_000 / rate);
        socket.write_all(&packet).await?;
        socket.read_u8().await?;
        wire.record(owner, FRAME as u64);
        total.fetch_add(FRAME as u64, Ordering::Relaxed);
        time::sleep_until(earliest).await;
    }
}

async fn measure(owner: &AtomicU64, public: &AtomicU64, period: Duration) -> (u64, u64) {
    let start = (
        owner.load(Ordering::Relaxed),
        public.load(Ordering::Relaxed),
    );
    let at = Instant::now();
    time::sleep(period).await;
    let ms = u64::try_from(at.elapsed().as_millis()).unwrap();
    (
        (owner.load(Ordering::Relaxed) - start.0) * 1000 / ms,
        (public.load(Ordering::Relaxed) - start.1) * 1000 / ms,
    )
}

#[tokio::test]
#[ignore = "wall-clock TCP contention evidence; run explicitly"]
async fn tcp_contention_reclaims_capacity_only_while_owner_benefits() {
    for use_hint in [false, true] {
        let (tx1, rx1) = socket_pair().await;
        let (tx2, rx2) = socket_pair().await;
        let wire = Arc::new(Egress::default());
        let owner = Arc::new(AtomicU64::new(0));
        let public = Arc::new(AtomicU64::new(0));
        let demand = Arc::new(AtomicU64::new(0));
        let cap = Arc::new(AtomicU64::new(LINK));
        let mut tasks = JoinSet::new();
        tasks.spawn(bottleneck([rx1, rx2]));
        tasks.spawn(send(tx1, true, demand.clone(), wire.clone(), owner.clone()));
        tasks.spawn(send(tx2, false, cap.clone(), wire.clone(), public.clone()));
        let (_, idle) = measure(&owner, &public, Duration::from_secs(2)).await;
        let hint = use_hint.then_some(idle);
        tasks.spawn(async move {
            let mut control = Preemption::default();
            control.configure(Some(LINK), hint);
            loop {
                let rate = control.sample(&wire, Instant::now()).unwrap();
                cap.store(rate, Ordering::Relaxed);
                time::sleep(SAMPLE).await;
            }
        });
        demand.store(LINK * 4, Ordering::Relaxed);
        time::sleep(Duration::from_secs(2)).await;
        let (bulk, shared) = measure(&owner, &public, Duration::from_secs(2)).await;
        demand.store(LINK / 8, Ordering::Relaxed);
        time::sleep(Duration::from_secs(3)).await;
        let (slow, restored) = measure(&owner, &public, Duration::from_secs(2)).await;
        eprintln!(
            "hint={hint:?} idle_public={idle} bulk_owner={bulk} bulk_public={shared} slow_owner={slow} slow_public={restored} B/s"
        );
        assert!(idle > LINK / 3 && bulk > idle * 6 / 10 && shared < idle / 2);
        assert!(
            slow < LINK / 5 && restored > idle.saturating_sub(slow) * 7 / 10 && restored > shared
        );
        tasks.shutdown().await;
    }
}
