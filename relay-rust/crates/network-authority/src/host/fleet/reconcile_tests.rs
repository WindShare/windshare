#![allow(
    clippy::future_not_send,
    reason = "Workers run JS futures on one isolate thread"
)]
use super::*;
use crate::directory_reconcile::{DirectoryMirror, ReconcileMode, reconcile};
use anyhow::anyhow;
use std::{
    cell::RefCell,
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
    let mirror = FakeMirror::default();

    assert!(block_on(reconcile(&mut store, &mirror, ReconcileMode::PendingOnly)).unwrap());
    assert_eq!(mirror.0.borrow().reads, 0);
    assert_eq!(mirror.0.borrow().writes, vec![h1.clone()]);

    // Simulate a stale 60 s KV edge cache entry right after publishing H1.
    // Non-directory-changing interactive calls must return immediately without KV I/O.
    mirror.0.borrow_mut().value = Some("stale-cached-h0".into());
    assert!(!block_on(reconcile(&mut store, &mirror, ReconcileMode::PendingOnly)).unwrap());
    assert_eq!(mirror.0.borrow().reads, 0);
    assert_eq!(mirror.0.borrow().writes, vec![h1]);
}

#[test]
fn audit_kv_skips_matching_copy_and_repairs_drift() {
    let (mut store, _signer, h1) = bootstrap_store();
    let mirror = FakeMirror::default();

    assert!(block_on(reconcile(&mut store, &mirror, ReconcileMode::PendingOnly)).unwrap());
    assert!(!block_on(reconcile(&mut store, &mirror, ReconcileMode::AuditKv)).unwrap());
    assert_eq!(mirror.0.borrow().reads, 1);
    assert_eq!(mirror.0.borrow().writes.len(), 1);

    mirror.0.borrow_mut().value = None;
    assert!(block_on(reconcile(&mut store, &mirror, ReconcileMode::AuditKv)).unwrap());
    assert_eq!(mirror.0.borrow().reads, 2);
    assert_eq!(mirror.0.borrow().writes, vec![h1.clone(), h1.clone()]);
    assert_eq!(mirror.0.borrow().value.as_deref(), Some(h1.as_str()));
}

#[test]
fn audit_kv_rereads_concurrent_head_after_await_and_coalesces_write() {
    let (mut store_a, signer, h1) = bootstrap_store();
    let mut store_b = store_a.clone();
    let mirror = FakeMirror::default();

    assert!(block_on(reconcile(&mut store_a, &mirror, ReconcileMode::PendingOnly)).unwrap());
    assert_eq!(mirror.0.borrow().writes, vec![h1]);

    // Stale KV read triggers drift repair, suspended while a concurrent mutation commits H2.
    {
        let mut state = mirror.0.borrow_mut();
        state.value = Some("stale-cached-h0".into());
        state.read_paused = true;
    }
    let mut audit = pin!(reconcile(&mut store_a, &mirror, ReconcileMode::AuditKv));
    assert!(poll_once(audit.as_mut()).is_pending());
    assert_eq!(mirror.0.borrow().reads, 1);

    let h2 = advance_head(&mut store_b, &signer, 4 * 86_400);
    mirror.0.borrow_mut().read_paused = false;

    assert!(matches!(poll_once(audit.as_mut()), Poll::Ready(Ok(true))));
    assert_eq!(mirror.0.borrow().writes.len(), 2);
    assert_eq!(mirror.0.borrow().writes[1], h2);
    assert_eq!(mirror.0.borrow().value.as_deref(), Some(h2.as_str()));

    // Request B wakes up on the publishing lock right after Request A and sees H2 already delivered.
    assert!(!block_on(reconcile(&mut store_b, &mirror, ReconcileMode::PendingOnly)).unwrap());
    assert_eq!(mirror.0.borrow().reads, 1);
    assert_eq!(mirror.0.borrow().writes.len(), 2);
}

#[test]
fn audit_kv_matching_read_leaves_concurrent_head_pending_for_next_reconcile() {
    let (mut store_a, signer, h1) = bootstrap_store();
    let mut store_b = store_a.clone();
    let mirror = FakeMirror::default();

    assert!(block_on(reconcile(&mut store_a, &mirror, ReconcileMode::PendingOnly)).unwrap());
    assert_eq!(mirror.0.borrow().value.as_deref(), Some(h1.as_str()));

    mirror.0.borrow_mut().read_paused = true;
    let mut audit = pin!(reconcile(&mut store_a, &mirror, ReconcileMode::AuditKv));
    assert!(poll_once(audit.as_mut()).is_pending());

    let h2 = advance_head(&mut store_b, &signer, 4 * 86_400);
    mirror.0.borrow_mut().read_paused = false;

    assert!(matches!(poll_once(audit.as_mut()), Poll::Ready(Ok(false))));
    assert_eq!(mirror.0.borrow().writes.len(), 1);

    assert!(block_on(reconcile(&mut store_b, &mirror, ReconcileMode::PendingOnly)).unwrap());
    assert_eq!(mirror.0.borrow().writes, vec![h1, h2]);
}

#[test]
fn failed_write_and_in_flight_commit_both_preserve_retry_correctness() {
    let (mut store_a, signer, h1) = bootstrap_store();
    let mut store_b = store_a.clone();
    let mirror = FakeMirror::default();

    // A failed KV write must not advance the delivery ledger.
    mirror.0.borrow_mut().fail_next_write = true;
    assert!(block_on(reconcile(&mut store_a, &mirror, ReconcileMode::PendingOnly)).is_err());
    assert_eq!(mirror.0.borrow().writes, Vec::<String>::new());

    // Pause while H1 is writing so H2 commits before H1 records DELIVERED.
    mirror.0.borrow_mut().write_paused = true;
    let mut publish_h1 = pin!(reconcile(&mut store_a, &mirror, ReconcileMode::PendingOnly));
    assert!(poll_once(publish_h1.as_mut()).is_pending());

    let h2 = advance_head(&mut store_b, &signer, 4 * 86_400);
    mirror.0.borrow_mut().write_paused = false;

    assert!(matches!(
        poll_once(publish_h1.as_mut()),
        Poll::Ready(Ok(true))
    ));
    assert_eq!(mirror.0.borrow().writes, vec![h1.clone()]);

    // Recording H1's digest must leave H2 pending so the following reconcile publishes H2.
    assert!(block_on(reconcile(&mut store_b, &mirror, ReconcileMode::PendingOnly)).unwrap());
    assert_eq!(mirror.0.borrow().writes, vec![h1, h2]);
}
