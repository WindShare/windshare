use super::*;

const TICK: Duration = Duration::from_millis(5);
const TICKS: usize = 40;

async fn serve(
    node: &Runtime,
    actors: impl IntoIterator<Item = Attribution>,
    frame: usize,
    reverse: bool,
) -> Vec<usize> {
    let mut streams: Vec<_> = (1u8..)
        .zip(actors)
        .map(|(id, attribution)| Stream::new(node, id, attribution, frame, BACKLOG))
        .collect();
    for stream in &mut streams {
        stream.serve_backlog();
    }
    let governor = node.governor.clone();
    let timer = tokio::spawn(async move { governor.run().await });
    tokio::task::yield_now().await;
    for _ in 0..TICKS {
        tokio::time::advance(TICK).await;
        tokio::task::yield_now().await;
        // A grant can wake another actor within this tick. Visit both actor orders
        // and drain those notifications before more pacing credit is earned.
        loop {
            let mut woke = false;
            for i in 0..streams.len() {
                let index = if reverse { streams.len() - i - 1 } else { i };
                let stream = &mut streams[index];
                if stream.wake.notified().now_or_never().is_some() {
                    woke = true;
                    stream.serve_backlog();
                }
            }
            if !woke {
                break;
            }
        }
    }
    timer.abort();
    let _ = timer.await;
    streams.into_iter().map(|stream| stream.sent).collect()
}

fn expected_frames(rate: usize, size: usize) -> usize {
    rate * TICKS * usize::try_from(TICK.as_millis()).unwrap() / 1_000 / size
}

#[tokio::test(start_paused = true)]
async fn paced_bursts_preserve_weighted_bytes_in_either_actor_order() {
    for (frame, rate) in [(FRAME, FRAME * 1_600), (16 << 10, 50_000_000)] {
        for reverse in [true, false] {
            let (_dir, node, [account, _]) = network(rate as u64);
            let sent = serve(&node, [account, Attribution::Anonymous], frame, reverse).await;
            let (standard, lowest) = (sent[0] / frame, sent[1] / frame);
            let total = expected_frames(rate, frame);
            assert_eq!(standard + lowest, total);
            assert!(
                lowest.abs_diff(total / 20) <= 1,
                "standard={standard}, lowest={lowest}, total={total}"
            );
        }
    }
}

#[tokio::test(start_paused = true)]
async fn paced_bursts_share_one_account_budget_across_eight_writers() {
    let frame = 16 << 10;
    let rate = 50_000_000;
    for reverse in [true, false] {
        let (_dir, node, [one, many]) = network(rate as u64);
        let accounts = std::iter::once(one).chain(std::iter::repeat_n(many, 8));
        let sent = serve(&node, accounts, frame, reverse).await;
        let single = sent[0] / frame;
        let other: Vec<_> = sent[1..].iter().map(|bytes| bytes / frame).collect();
        let many = other.iter().sum::<usize>();
        assert_eq!(single + many, expected_frames(rate, frame));
        assert!(single.abs_diff(many) <= 1, "single={single}, eight={many}");
        assert!(other.iter().max().unwrap() - other.iter().min().unwrap() <= 1);
    }
}

#[tokio::test(start_paused = true)]
async fn a_nonresponsive_actor_yields_its_turn_at_the_next_refill() {
    let (_dir, node, _) = network(8 * FRAME as u64 * 200);
    let mut owner = Stream::new(&node, 1, OWNER, FRAME, BACKLOG);
    let mut public = Stream::new(&node, 2, Attribution::Anonymous, FRAME, BACKLOG);
    assert!(!owner.submit());
    assert!(!public.submit());
    tokio::time::advance(TICK).await;
    assert!(!public.submit());
    assert!(owner.wake.notified().now_or_never().is_some());
    // Ignore that wake: other sockets must be able to spend the remaining credit.
    tokio::time::advance(TICK).await;
    public.serve_backlog();
    assert!(public.sent >= 8 * FRAME);
    assert!(owner.submit());
    drop((owner, public));
}

#[tokio::test(start_paused = true)]
async fn disabling_pacing_releases_reservations_and_wakes_throttled_writers() {
    let (_dir, node, _) = network(FRAME as u64 * 200);
    let mut waiting = Stream::new(&node, 1, Attribution::Anonymous, FRAME, BACKLOG);
    let mut other = Stream::new(&node, 2, Attribution::Anonymous, FRAME, BACKLOG);
    assert!(!waiting.submit());
    assert!(!other.submit());
    tokio::time::advance(TICK).await;
    assert!(!other.submit());
    assert!(waiting.wake.notified().now_or_never().is_some());
    node.update(Settings::default(), 0).unwrap();
    assert!(waiting.wake.notified().now_or_never().is_some());
    assert!(other.wake.notified().now_or_never().is_some());
    waiting.drain();
    other.drain();
    assert_eq!(
        (waiting.sent, other.sent),
        (BACKLOG * FRAME, BACKLOG * FRAME)
    );
}
