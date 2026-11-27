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

fn step(
    control: &mut Preemption,
    wire: &Egress,
    now: &mut Instant,
    owner_rate: u64,
    public_rate: u64,
) -> u64 {
    wire.record(true, owner_rate / 4);
    wire.record(false, public_rate / 4);
    *now += SAMPLE;
    control.sample(wire, *now).expect("preemption enabled")
}

fn enter_protected_hold(
    control: &mut Preemption,
    wire: &Egress,
    now: &mut Instant,
    cap: u64,
    initial_owner: u64,
    peak_owner: u64,
) -> u64 {
    assert_eq!(control.sample(wire, *now), Some(cap));
    // Initial probe cuts public to half of cap.
    let probed = step(control, wire, now, initial_owner, cap);
    assert_eq!(probed, cap / 2);
    // Probe succeeds when owner claims a material share of the reclaimed capacity.
    assert_eq!(step(control, wire, now, peak_owner, probed), probed);
    // Once public drops to the signal floor, Tracking settles into Hold.
    assert_eq!(step(control, wire, now, peak_owner, SIGNAL_FLOOR), probed);
    probed
}

fn hold_until_expiry(
    control: &mut Preemption,
    wire: &Egress,
    now: &mut Instant,
    owner_rate: u64,
    expected_limit: u64,
) {
    // PROBE_INTERVAL (2 s) spans 8 SAMPLE ticks (250 ms); the first 7 hold.
    for _ in 0..7 {
        assert_eq!(
            step(control, wire, now, owner_rate, expected_limit),
            expected_limit
        );
    }
}

#[test]
fn restore_confirmation_survives_transient_owner_recovery_and_refreshes_protected_rate() {
    let cap = 512_000;
    let mut control = Preemption::default();
    let wire = Egress::default();
    let mut now = Instant::now();
    assert!(control.configure(Some(cap), None));

    let retained = enter_protected_hold(&mut control, &wire, &mut now, cap, 256_000, 500_000);
    // Owner settles modestly lower during Hold (still above 500k * 2/3) before Hold expires.
    hold_until_expiry(&mut control, &wire, &mut now, 380_000, retained);
    assert_eq!(step(&mut control, &wire, &mut now, 380_000, retained), cap);
    // Lifting public to full cap drops owner by >10%, so Restore re-engages retained.
    assert_eq!(step(&mut control, &wire, &mut now, 260_000, cap), retained);
    // During the brief post-Restore recovery window, depressed owner throughput does not
    // immediately cancel the protection Restore just confirmed.
    assert_eq!(
        step(&mut control, &wire, &mut now, 200_000, retained),
        retained
    );
    assert_eq!(
        step(&mut control, &wire, &mut now, 240_000, retained),
        retained
    );
    // After recovery, 300k is below the stale 500k * 2/3 peak but well above the refreshed
    // 380k * 2/3 baseline, so protection remains active until the new Hold expires.
    assert_eq!(
        step(&mut control, &wire, &mut now, 300_000, retained),
        retained
    );
}

#[test]
fn post_restore_recovery_releases_promptly_when_owner_stops_or_stays_slow() {
    let cap = 512_000;

    // Owner stops immediately after Restore confirms contention -> instant release.
    let mut idle_case = Preemption::default();
    let wire = Egress::default();
    let mut now = Instant::now();
    idle_case.configure(Some(cap), None);
    let retained = enter_protected_hold(&mut idle_case, &wire, &mut now, cap, 256_000, 480_000);
    hold_until_expiry(&mut idle_case, &wire, &mut now, 480_000, retained);
    assert_eq!(
        step(&mut idle_case, &wire, &mut now, 480_000, retained),
        cap
    );
    assert_eq!(
        step(&mut idle_case, &wire, &mut now, 300_000, cap),
        retained
    );
    assert_eq!(step(&mut idle_case, &wire, &mut now, 0, retained), cap);

    // Owner stays stuck at a slow remote bottleneck (above ACTIVE_RATE, below 2/3) ->
    // releases right after the bounded recovery grace.
    let mut slow_case = Preemption::default();
    let mut now = Instant::now();
    slow_case.configure(Some(cap), None);
    let retained = enter_protected_hold(&mut slow_case, &wire, &mut now, cap, 256_000, 480_000);
    hold_until_expiry(&mut slow_case, &wire, &mut now, 480_000, retained);
    assert_eq!(
        step(&mut slow_case, &wire, &mut now, 480_000, retained),
        cap
    );
    assert_eq!(
        step(&mut slow_case, &wire, &mut now, 300_000, cap),
        retained
    );
    assert_eq!(
        step(&mut slow_case, &wire, &mut now, 80_000, retained),
        retained
    );
    assert_eq!(
        step(&mut slow_case, &wire, &mut now, 80_000, retained),
        retained
    );
    assert_eq!(step(&mut slow_case, &wire, &mut now, 80_000, retained), cap);

    // Fast recovery exits Recover early so a subsequent slowdown releases immediately.
    let mut fast_case = Preemption::default();
    let mut now = Instant::now();
    fast_case.configure(Some(cap), None);
    let retained = enter_protected_hold(&mut fast_case, &wire, &mut now, cap, 256_000, 480_000);
    hold_until_expiry(&mut fast_case, &wire, &mut now, 480_000, retained);
    assert_eq!(
        step(&mut fast_case, &wire, &mut now, 480_000, retained),
        cap
    );
    assert_eq!(
        step(&mut fast_case, &wire, &mut now, 300_000, cap),
        retained
    );
    assert_eq!(
        step(&mut fast_case, &wire, &mut now, 460_000, retained),
        retained
    );
    assert_eq!(step(&mut fast_case, &wire, &mut now, 80_000, retained), cap);
}

#[test]
fn restore_release_clears_protected_rate_for_subsequent_probes() {
    let cap = 512_000;
    let mut control = Preemption::default();
    let wire = Egress::default();
    let mut now = Instant::now();
    control.configure(Some(cap), None);

    // Episode 1 establishes a high protected rate (600k), then Restore sees no drop and releases.
    let retained = enter_protected_hold(&mut control, &wire, &mut now, cap, 300_000, 600_000);
    hold_until_expiry(&mut control, &wire, &mut now, 600_000, retained);
    assert_eq!(step(&mut control, &wire, &mut now, 600_000, retained), cap);
    assert_eq!(step(&mut control, &wire, &mut now, 600_000, cap), cap);

    // After Cooldown expires with a slower active owner (250k < 600k * 2/3), a new Probe
    // that delivers a genuine owner gain (250k -> 340k) must succeed instead of being aborted
    // by Episode 1's stale protected_rate.
    hold_until_expiry(&mut control, &wire, &mut now, 250_000, cap);
    let probed = step(&mut control, &wire, &mut now, 250_000, cap);
    assert_eq!(probed, cap / 2);
    assert_eq!(step(&mut control, &wire, &mut now, 340_000, probed), probed);
}

#[test]
fn first_sample_and_reconfigure_drain_stale_egress_counters() {
    let cap = 1_000_000;
    let mut control = Preemption::default();
    let wire = Egress::default();
    let mut now = Instant::now();
    control.configure(Some(cap), None);

    // Pre-sample bytes recorded before the first window opens must not create a phantom spike.
    wire.record(true, 10_000_000);
    wire.record(false, 10_000_000);
    assert_eq!(control.sample(&wire, now), Some(cap));
    assert_eq!(step(&mut control, &wire, &mut now, 0, cap), cap);

    // Mid-window bytes recorded before reconfigure must not leak into the next window either.
    wire.record(true, 5_000_000);
    wire.record(false, 5_000_000);
    assert!(control.configure(Some(cap / 2), None));
    assert_eq!(control.sample(&wire, now), Some(cap / 2));
    assert_eq!(step(&mut control, &wire, &mut now, 0, cap / 2), cap / 2);
}

#[test]
fn hinted_ramp_clamps_backoff_to_uplink_when_uplink_is_below_cap() {
    let wire = Egress::default();
    let mut now = Instant::now();

    // Small uplink below the default public cap must still back off to uplink / 4.
    let mut clamped = Preemption::default();
    clamped.configure(Some(3_125_000), Some(500_000));
    assert_eq!(clamped.sample(&wire, now), Some(3_125_000));
    assert_eq!(
        step(&mut clamped, &wire, &mut now, 100_000, 500_000),
        125_000
    );
    // Tracking then leaves uplink - owner - 5% margin.
    assert_eq!(
        step(&mut clamped, &wire, &mut now, 400_000, 125_000),
        75_000
    );

    // When uplink exceeds cap, Ramp preserves cap / 4.
    let mut uncapped = Preemption::default();
    uncapped.configure(Some(1_000_000), Some(4_000_000));
    assert_eq!(uncapped.sample(&wire, now), Some(1_000_000));
    assert_eq!(
        step(&mut uncapped, &wire, &mut now, 200_000, 1_000_000),
        250_000
    );
}

#[test]
fn saturated_owner_rate_does_not_overflow_release_threshold() {
    let cap = 512_000;
    let mut control = Preemption::default();
    let wire = Egress::default();
    let mut now = Instant::now();
    control.configure(Some(cap), None);
    assert_eq!(control.sample(&wire, now), Some(cap));

    let probed = step(&mut control, &wire, &mut now, 256_000, cap);
    assert_eq!(probed, cap / 2);

    // Saturate rate_per_second at u64::MAX so protected_rate becomes u64::MAX.
    wire.record(true, u64::MAX);
    wire.record(false, SIGNAL_FLOOR / 4);
    now += SAMPLE;
    assert_eq!(control.sample(&wire, now), Some(probed));

    // Evaluating the 2/3 release threshold against u64::MAX must neither overflow nor release.
    wire.record(true, u64::MAX);
    wire.record(false, SIGNAL_FLOOR / 4);
    now += SAMPLE;
    assert_eq!(control.sample(&wire, now), Some(probed));

    // A genuinely lower rate cleanly triggers release without overflowing.
    assert_eq!(
        step(&mut control, &wire, &mut now, 1_000_000, SIGNAL_FLOOR),
        cap
    );
}
