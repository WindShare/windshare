use super::{TICK, WritePermit, WriteWindow};
use std::{collections::VecDeque, sync::Arc};
use tokio::{sync::Notify, time::Instant};

/// The Arc stays alive while any lane can refer to this destination.
#[derive(Clone, Copy, Eq, PartialEq, Ord, PartialOrd)]
pub(super) struct WriterId(usize);

impl From<&WriteWindow> for WriterId {
    fn from(window: &WriteWindow) -> Self {
        Self(std::ptr::from_ref(window).addr())
    }
}

struct Grant {
    lane: u64,
    bytes: u64,
    issued: Instant,
}

pub(super) struct Destination {
    pub(super) window: Arc<WriteWindow>,
    pub(super) wake: Arc<Notify>,
    pub(super) residents: usize,
    grants: VecDeque<Grant>,
}

impl Destination {
    pub(super) const fn new(window: Arc<WriteWindow>, wake: Arc<Notify>) -> Self {
        Self {
            window,
            wake,
            residents: 0,
            grants: VecDeque::new(),
        }
    }

    /// Unclaimed grants mean the actor has not handed the frames to the socket
    /// yet. Preserve its turn for one refill interval, even with a large burst.
    pub(super) fn awaiting_actor(&self, now: Instant) -> bool {
        self.grants
            .front()
            .is_some_and(|g| now.duration_since(g.issued) < TICK)
    }

    pub(super) fn fits(&self, bytes: u64) -> bool {
        // Only the destination actor can consume a slot. Count its outstanding
        // promises as well as frames already handed to the socket.
        let promised_bytes: u64 = self.grants.iter().map(|grant| grant.bytes).sum();
        self.window
            .fits(self.grants.len() + 1, promised_bytes + bytes)
    }

    pub(super) fn grant(&mut self, lane: u64, bytes: u64, issued: Instant) {
        self.grants.push_back(Grant {
            lane,
            bytes,
            issued,
        });
    }

    pub(super) fn take(&mut self) -> Option<(u64, WritePermit)> {
        let grant = self.grants.pop_front()?;
        let permit = self
            .window
            .reserve(grant.bytes)
            .expect("promised writer capacity");
        Some((grant.lane, permit))
    }

    pub(super) fn discard(&mut self, lane: u64) {
        let previous = self.grants.len();
        self.grants.retain(|g| g.lane != lane);
        if previous != self.grants.len() {
            self.wake.notify_one();
        }
    }

    pub(super) fn reset(&mut self) {
        self.grants.clear();
        if self
            .window
            .scheduled
            .swap(false, std::sync::atomic::Ordering::Relaxed)
        {
            self.wake.notify_one();
        }
    }
}
