#![allow(
    clippy::future_not_send,
    reason = "Workers run JS futures on one isolate thread"
)]
use super::*;
use crate::directory_reconcile::{
    AUDIT_SETTLE_MS, DirectoryMirror, RETRY_BACKOFF_MS, ReconcileMode, ReconcileOutcome,
    WRITE_COOLDOWN_MS, reconcile,
};
use anyhow::anyhow;
use std::{
    cell::{Cell, RefCell},
    future::Future,
    pin::{Pin, pin},
    rc::Rc,
    task::{Context, Poll, Waker},
};

#[derive(Clone, Default)]
struct SharedStore(Rc<RefCell<Memory>>);

impl Store for SharedStore {
    fn get(&self, key: &str) -> Result<Option<String>> {
        self.0.borrow().get(key)
    }

    fn put(&mut self, key: &str, value: &str) -> Result<()> {
        self.0.borrow_mut().put(key, value)
    }

    fn delete(&mut self, key: &str) -> Result<()> {
        self.0.borrow_mut().delete(key)
    }

    fn prune_registration_proofs(&mut self, kind: ProofKind, now: u64) -> Result<()> {
        self.0.borrow_mut().prune_registration_proofs(kind, now)
    }

    fn reports_after(
        &self,
        after: Option<crate::ReportPosition>,
        limit: usize,
    ) -> Result<Vec<crate::Report>> {
        self.0.borrow().reports_after(after, limit)
    }

    fn scan_after(
        &self,
        prefix: &str,
        after: Option<&str>,
        limit: Option<usize>,
    ) -> Result<Vec<(String, String)>> {
        self.0.borrow().scan_after(prefix, after, limit)
    }
}

#[derive(Default)]
struct MirrorState {
    value: Option<String>,
    reads: usize,
    writes: Vec<String>,
    read_paused: bool,
    write_paused: bool,
    fail_next_write: bool,
}

#[derive(Clone, Default)]
struct FakeMirror(Rc<RefCell<MirrorState>>);

struct GatedRead {
    state: Rc<RefCell<MirrorState>>,
    counted: bool,
}

impl Future for GatedRead {
    type Output = Result<Option<String>>;

    fn poll(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<Self::Output> {
        let this = self.get_mut();
        let mut state = this.state.borrow_mut();
        if !this.counted {
            state.reads += 1;
            this.counted = true;
        }
        if state.read_paused {
            return Poll::Pending;
        }
        Poll::Ready(Ok(state.value.clone()))
    }
}

struct GatedWrite {
    state: Rc<RefCell<MirrorState>>,
    body: String,
}

impl Future for GatedWrite {
    type Output = Result<()>;

    fn poll(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<Self::Output> {
        let mut state = self.state.borrow_mut();
        if state.write_paused {
            return Poll::Pending;
        }
        if state.fail_next_write {
            state.fail_next_write = false;
            return Poll::Ready(Err(anyhow!("KV 429 rate limited")));
        }
        state.value = Some(self.body.clone());
        state.writes.push(self.body.clone());
        Poll::Ready(Ok(()))
    }
}

impl DirectoryMirror for FakeMirror {
    fn read(&self) -> impl Future<Output = Result<Option<String>>> {
        GatedRead {
            state: Rc::clone(&self.0),
            counted: false,
        }
    }

    fn write(&self, body: &str) -> impl Future<Output = Result<()>> {
        GatedWrite {
            state: Rc::clone(&self.0),
            body: body.to_owned(),
        }
    }
}

#[derive(Clone)]
struct TestClock(Rc<Cell<u64>>);

impl TestClock {
    fn new(start_ms: u64) -> Self {
        Self(Rc::new(Cell::new(start_ms)))
    }

    fn now(&self) -> u64 {
        self.0.get()
    }

    fn set(&self, at_ms: u64) {
        self.0.set(at_ms);
    }

    fn advance(&self, delta_ms: u64) {
        self.0.set(self.0.get().saturating_add(delta_ms));
    }

    fn reader(&self) -> impl FnMut() -> u64 + '_ {
        || self.now()
    }
}

fn poll_once<F: Future>(future: Pin<&mut F>) -> Poll<F::Output> {
    let mut cx = Context::from_waker(Waker::noop());
    future.poll(&mut cx)
}

fn block_on<F: Future>(future: F) -> F::Output {
    let mut future = pin!(future);
    match poll_once(future.as_mut()) {
        Poll::Ready(output) => output,
        Poll::Pending => panic!("unexpected pending future"),
    }
}

fn bootstrap_store() -> (SharedStore, Publisher, String) {
    let mut store = SharedStore::default();
    let (release, signer, history) = directory();
    let head = Publication::bootstrap(&mut store, &release, &[&history]).unwrap();
    let body = serde_json::to_string(&head.envelope).unwrap();
    (store, signer, body)
}

fn advance_head(store: &mut SharedStore, signer: &Publisher, now: u64) -> String {
    let mut head = crate::fleet::publication::current(store).unwrap();
    assert!(head.renew(signer, now).unwrap());
    head.save(store).unwrap();
    serde_json::to_string(&head.envelope).unwrap()
}

#[test]
fn pending_only_publishes_new_head_and_skips_kv_audit_on_unchanged_head() {
    let (mut store, _signer, h1) = bootstrap_store();
    let digest = crate::fleet::publication::current(&store)
        .unwrap()
        .revision()
        .unwrap()
        .digest;
    assert!(crate::directory_reconcile::sync_pending(&store, &digest).unwrap());
    let mirror = FakeMirror::default();
    let clock = TestClock::new(10_000);

    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );
    assert!(!crate::directory_reconcile::sync_pending(&store, &digest).unwrap());
    assert_eq!(ReconcileOutcome::Published.retry_after_ms(), None);
    assert_eq!(mirror.0.borrow().reads, 0);
    assert_eq!(mirror.0.borrow().writes, vec![h1.clone()]);

    // Simulate a stale 60 s KV edge cache entry right after publishing H1.
    // Non-directory-changing interactive calls must return immediately without KV I/O.
    mirror.0.borrow_mut().value = Some("stale-cached-h0".into());
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Settling
    );
    clock.advance(AUDIT_SETTLE_MS);
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::UpToDate
    );
    assert_eq!(mirror.0.borrow().reads, 0);
    assert_eq!(mirror.0.borrow().writes, vec![h1]);
}

#[test]
fn audit_kv_skips_during_settling_window_and_repairs_drift_after_settling() {
    let (mut store, _signer, h1) = bootstrap_store();
    let mirror = FakeMirror::default();
    let clock = TestClock::new(10_000);

    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );

    // Within 120 s of a KV write, a stale 60 s edge-cache copy must not trigger
    // a KV read or phantom audit_repair write.
    mirror.0.borrow_mut().value = Some("stale-cached-h0".into());
    clock.advance(AUDIT_SETTLE_MS - 1);
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::AuditKv,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Settling
    );
    assert_eq!(mirror.0.borrow().reads, 0);
    assert_eq!(mirror.0.borrow().writes.len(), 1);

    // Once the settling window elapses, matching KV returns UpToDate.
    mirror.0.borrow_mut().value = Some(h1.clone());
    clock.advance(1);
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::AuditKv,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::UpToDate
    );
    assert_eq!(mirror.0.borrow().reads, 1);
    assert_eq!(mirror.0.borrow().writes.len(), 1);

    // Real drift after the settling window repairs KV and starts a fresh settling window.
    mirror.0.borrow_mut().value = None;
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::AuditKv,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );
    assert_eq!(mirror.0.borrow().reads, 2);
    assert_eq!(mirror.0.borrow().writes, vec![h1.clone(), h1.clone()]);
    assert_eq!(mirror.0.borrow().value.as_deref(), Some(h1.as_str()));

    mirror.0.borrow_mut().value = Some("stale-again".into());
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::AuditKv,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Settling
    );
    assert_eq!(mirror.0.borrow().reads, 2);
    assert_eq!(mirror.0.borrow().writes.len(), 2);
}

#[test]
fn write_cooldown_defers_rapid_mutations_and_coalesces_into_one_write() {
    let (mut store, signer, h1) = bootstrap_store();
    let mirror = FakeMirror::default();
    let clock = TestClock::new(10_000);

    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );

    // A second mutation within WRITE_COOLDOWN_MS defers instead of hitting KV 429.
    let _h2 = advance_head(&mut store, &signer, 4 * 86_400);
    clock.advance(400);
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Cooldown {
            retry_after_ms: WRITE_COOLDOWN_MS - 400
        }
    );
    assert_eq!(mirror.0.borrow().writes, vec![h1.clone()]);

    // Another mutation during the same cooldown window still skips KV and coalesces.
    let h3 = advance_head(&mut store, &signer, 88 * 86_400);
    clock.advance(500);
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::AuditKv,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Cooldown {
            retry_after_ms: WRITE_COOLDOWN_MS - 900
        }
    );
    assert_eq!(mirror.0.borrow().reads, 0);
    assert_eq!(mirror.0.borrow().writes, vec![h1.clone()]);

    // Once cooldown elapses, one alarm retry publishes the newest head directly.
    clock.advance(WRITE_COOLDOWN_MS - 900);
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );
    assert_eq!(mirror.0.borrow().writes, vec![h1, h3]);
}

#[test]
fn audit_kv_rereads_concurrent_head_after_await_and_coalesces_write() {
    let (mut store_a, signer, h1) = bootstrap_store();
    let mut store_b = store_a.clone();
    let mirror = FakeMirror::default();
    let clock = TestClock::new(10_000);

    assert_eq!(
        block_on(reconcile(
            &mut store_a,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );
    assert_eq!(mirror.0.borrow().writes, vec![h1]);
    clock.advance(AUDIT_SETTLE_MS);

    // Drift repair suspends on KV read while a concurrent mutation commits H2.
    {
        let mut state = mirror.0.borrow_mut();
        state.value = Some("stale-cached-h0".into());
        state.read_paused = true;
    }
    let mut audit = pin!(reconcile(
        &mut store_a,
        &mirror,
        ReconcileMode::AuditKv,
        clock.reader(),
    ));
    assert!(poll_once(audit.as_mut()).is_pending());
    assert_eq!(mirror.0.borrow().reads, 1);

    let h2 = advance_head(&mut store_b, &signer, 4 * 86_400);
    mirror.0.borrow_mut().read_paused = false;

    assert!(matches!(
        poll_once(audit.as_mut()),
        Poll::Ready(Ok(ReconcileOutcome::Published))
    ));
    assert_eq!(mirror.0.borrow().writes.len(), 2);
    assert_eq!(mirror.0.borrow().writes[1], h2);
    assert_eq!(mirror.0.borrow().value.as_deref(), Some(h2.as_str()));

    // Request B wakes up on the publishing lock right after Request A and sees H2 already delivered.
    assert_eq!(
        block_on(reconcile(
            &mut store_b,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Settling
    );
    assert_eq!(mirror.0.borrow().reads, 1);
    assert_eq!(mirror.0.borrow().writes.len(), 2);
}

#[test]
fn audit_kv_matching_read_leaves_concurrent_head_pending_for_next_reconcile() {
    let (mut store_a, signer, h1) = bootstrap_store();
    let mut store_b = store_a.clone();
    let mirror = FakeMirror::default();
    let clock = TestClock::new(10_000);

    assert_eq!(
        block_on(reconcile(
            &mut store_a,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );
    assert_eq!(mirror.0.borrow().value.as_deref(), Some(h1.as_str()));
    clock.advance(AUDIT_SETTLE_MS);

    mirror.0.borrow_mut().read_paused = true;
    let mut audit = pin!(reconcile(
        &mut store_a,
        &mirror,
        ReconcileMode::AuditKv,
        clock.reader(),
    ));
    assert!(poll_once(audit.as_mut()).is_pending());

    let h2 = advance_head(&mut store_b, &signer, 4 * 86_400);
    mirror.0.borrow_mut().read_paused = false;

    assert!(matches!(
        poll_once(audit.as_mut()),
        Poll::Ready(Ok(ReconcileOutcome::UpToDate))
    ));
    assert_eq!(mirror.0.borrow().writes.len(), 1);

    assert_eq!(
        block_on(reconcile(
            &mut store_b,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );
    assert_eq!(mirror.0.borrow().writes, vec![h1, h2]);
}

#[test]
fn failed_write_uses_bounded_backoff_and_resets_after_recovery() {
    let (mut store, signer, h1) = bootstrap_store();
    let mirror = FakeMirror::default();
    let clock = TestClock::new(10_000);

    for (index, expected_delay) in RETRY_BACKOFF_MS.into_iter().enumerate() {
        mirror.0.borrow_mut().fail_next_write = true;
        assert_eq!(
            block_on(reconcile(
                &mut store,
                &mirror,
                ReconcileMode::PendingOnly,
                clock.reader(),
            ))
            .unwrap(),
            ReconcileOutcome::RetryPending {
                retry_after_ms: Some(expected_delay),
                attempts: u8::try_from(index + 1).unwrap(),
            }
        );
    }

    // Exhausting the bounded alarm schedule stops arming alarms and hands off to cron ticks.
    mirror.0.borrow_mut().fail_next_write = true;
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::RetryPending {
            retry_after_ms: None,
            attempts: u8::try_from(RETRY_BACKOFF_MS.len() + 1).unwrap(),
        }
    );
    assert_eq!(mirror.0.borrow().writes, Vec::<String>::new());

    // A subsequent write succeeds and resets the retry budget for future failures.
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::AuditKv,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );
    assert_eq!(mirror.0.borrow().writes, vec![h1]);

    let _h2 = advance_head(&mut store, &signer, 4 * 86_400);
    clock.advance(WRITE_COOLDOWN_MS);
    mirror.0.borrow_mut().fail_next_write = true;
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::RetryPending {
            retry_after_ms: Some(RETRY_BACKOFF_MS[0]),
            attempts: 1,
        }
    );
}

#[test]
fn failed_audit_repair_clears_delivery_record_so_pending_only_alarm_can_repair() {
    let (mut store, _signer, h1) = bootstrap_store();
    let mirror = FakeMirror::default();
    let clock = TestClock::new(10_000);

    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );
    clock.advance(AUDIT_SETTLE_MS);
    mirror.0.borrow_mut().value = None;
    mirror.0.borrow_mut().fail_next_write = true;

    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::AuditKv,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::RetryPending {
            retry_after_ms: Some(RETRY_BACKOFF_MS[0]),
            attempts: 1,
        }
    );

    // Alarm retries run in PendingOnly mode and must still repair KV without re-reading it.
    clock.advance(RETRY_BACKOFF_MS[0]);
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );
    assert_eq!(mirror.0.borrow().reads, 1);
    assert_eq!(mirror.0.borrow().writes, vec![h1.clone(), h1]);
}

#[test]
fn in_flight_commit_during_write_defers_with_cooldown_and_publishes_after_cooldown() {
    let (mut store_a, signer, h1) = bootstrap_store();
    let mut store_b = store_a.clone();
    let mirror = FakeMirror::default();
    let clock = TestClock::new(10_000);

    // Pause while H1 is writing so H2 commits before H1 records delivery.
    mirror.0.borrow_mut().write_paused = true;
    let mut publish_h1 = pin!(reconcile(
        &mut store_a,
        &mirror,
        ReconcileMode::PendingOnly,
        clock.reader(),
    ));
    assert!(poll_once(publish_h1.as_mut()).is_pending());

    let h2 = advance_head(&mut store_b, &signer, 4 * 86_400);
    clock.advance(150);
    mirror.0.borrow_mut().write_paused = false;

    assert!(matches!(
        poll_once(publish_h1.as_mut()),
        Poll::Ready(Ok(ReconcileOutcome::Published))
    ));
    assert_eq!(mirror.0.borrow().writes, vec![h1.clone()]);

    // Recording H1's post-write timestamp leaves H2 pending behind the cooldown window.
    assert_eq!(
        block_on(reconcile(
            &mut store_b,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Cooldown {
            retry_after_ms: WRITE_COOLDOWN_MS
        }
    );
    clock.advance(WRITE_COOLDOWN_MS);
    assert_eq!(
        block_on(reconcile(
            &mut store_b,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );
    assert_eq!(mirror.0.borrow().writes, vec![h1, h2]);
}

#[test]
fn backward_clock_step_does_not_wedge_cooldown_or_audit() {
    let (mut store, signer, h1) = bootstrap_store();
    let mirror = FakeMirror::default();
    let clock = TestClock::new(50_000);

    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );

    // Stepping the clock backward must not suppress AuditKv forever or wedge new writes.
    clock.set(1_000);
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::AuditKv,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::UpToDate
    );
    assert_eq!(mirror.0.borrow().reads, 1);

    let h2 = advance_head(&mut store, &signer, 4 * 86_400);
    assert_eq!(
        block_on(reconcile(
            &mut store,
            &mirror,
            ReconcileMode::PendingOnly,
            clock.reader(),
        ))
        .unwrap(),
        ReconcileOutcome::Published
    );
    assert_eq!(mirror.0.borrow().writes, vec![h1, h2]);
}
