use super::handoff::{Destination, WriterId};
use super::{Bandwidth, Key, MAX_FRAME, MAX_SESSIONS, Service, WritePermit, WriteWindow};
use crate::config::SUBMITTED_DATA_FRAMES;
use std::sync::atomic::Ordering;
use std::{
    collections::{BTreeMap, VecDeque},
    sync::Arc,
};
use tokio::{sync::Notify, time::Instant};
use windshare_relay_access::{
    Attribution, Principal,
    entitlement::Class,
    governor::{Demand, Scheduler},
};

enum Unavailable {
    Writer(Key),
    Bandwidth,
    Handoff,
}

struct Lane {
    session: Key,
    attribution: Attribution,
    writer: WriterId,
    heads: VecDeque<u64>,
    unclaimed: usize,
    queued: bool,
}

#[derive(Default)]
struct Session {
    directions: VecDeque<u64>,
    residents: usize,
}

pub(super) struct State {
    scheduler: Scheduler<Key>,
    sessions: BTreeMap<Key, Session>,
    lanes: BTreeMap<u64, Lane>,
    next_lane: u64,
    writers: BTreeMap<WriterId, Destination>,
    pub(super) pacing: Bandwidth,
    classified: u64,
}

impl State {
    pub(super) fn new() -> Self {
        Self {
            scheduler: Scheduler::new(MAX_SESSIONS, MAX_FRAME),
            sessions: BTreeMap::new(),
            lanes: BTreeMap::new(),
            next_lane: 0,
            writers: BTreeMap::new(),
            pacing: Bandwidth::default(),
            classified: 0,
        }
    }

    pub(super) fn register(
        &mut self,
        session: Key,
        attribution: Attribution,
        wake: Arc<Notify>,
        window: Arc<WriteWindow>,
    ) -> Option<u64> {
        if self.lanes.len() >= MAX_SESSIONS * 2
            || (self.sessions.len() >= MAX_SESSIONS && !self.sessions.contains_key(&session))
        {
            return None;
        }
        self.next_lane = self.next_lane.checked_add(1)?;
        let id = self.next_lane;
        let writer = WriterId::from(window.as_ref());
        self.writers
            .entry(writer)
            .or_insert_with(|| Destination::new(window, wake))
            .residents += 1;
        self.sessions.entry(session).or_default().residents += 1;
        self.lanes.insert(
            id,
            Lane {
                session,
                attribution,
                writer,
                heads: VecDeque::new(),
                unclaimed: 0,
                queued: false,
            },
        );
        Some(id)
    }

    pub(super) fn refresh_classes(
        &mut self,
        access: &Service,
        second: u64,
    ) -> Vec<(Principal, Class, Class)> {
        if self.classified == second {
            return Vec::new();
        }
        access.expire_quotas(second);
        let mut reclassified = Vec::new();
        for lane in self.lanes.values() {
            let principal = lane.attribution.principal(lane.session.0);
            let to = access.class(lane.attribution, second).0;
            if let Some(from) = self.scheduler.reclassify(principal, to) {
                reclassified.push((principal, from, to));
            }
        }
        self.classified = second;
        reclassified
    }

    pub(super) fn offer(
        &mut self,
        id: u64,
        sizes: impl Iterator<Item = u64>,
        access: &Service,
        now: Instant,
    ) {
        let lane = self.lanes.get_mut(&id).expect("live lane");
        // Published frames stay in the actor FIFO until it takes their grants.
        let known = lane.unclaimed + lane.heads.len();
        lane.heads
            .extend(sizes.take(SUBMITTED_DATA_FRAMES + 1).skip(known));
        if !lane.heads.is_empty() {
            self.writers[&lane.writer]
                .window
                .scheduled
                .store(true, Ordering::Relaxed);
        }
        let session = lane.session;
        if self.enqueue(id, now) {
            self.publish_head(session, access);
        }
    }

    /// Returns true when the session has gained its first competing direction.
    fn enqueue(&mut self, id: u64, now: Instant) -> bool {
        let lane = self.lanes.get_mut(&id).expect("live lane");
        let destination = &self.writers[&lane.writer];
        if lane.queued
            || !lane
                .heads
                .front()
                .is_some_and(|&bytes| destination.fits(bytes) || destination.awaiting_actor(now))
        {
            return false;
        }
        let directions = &mut self
            .sessions
            .get_mut(&lane.session)
            .expect("live session")
            .directions;
        lane.queued = true;
        directions.push_back(id);
        directions.len() == 1
    }

    fn publish_head(&mut self, session: Key, access: &Service) {
        if let Some(id) = self.sessions[&session].directions.front() {
            let lane = &self.lanes[id];
            let demand = Demand {
                principal: lane.attribution.principal(session.0),
                class: access.class(lane.attribution, self.classified).0,
                bytes: lane.heads[0],
            };
            assert!(self.scheduler.request(session, demand));
        }
    }

    pub(super) fn grant_pending(
        &mut self,
        access: &Service,
        caller: Option<WriterId>,
        now: Instant,
    ) {
        let available = self.pacing.replenish(now);
        let mut remaining = available;
        loop {
            let result = self.scheduler.try_next(|session, class, bytes| {
                let id = self.sessions[&session].directions[0];
                let destination = &self.writers[&self.lanes[&id].writer];
                if !destination.fits(bytes) {
                    return Err(if destination.awaiting_actor(now) {
                        Unavailable::Handoff
                    } else {
                        Unavailable::Writer(session)
                    });
                }
                remaining
                    .reserve(class, bytes)
                    .then_some(())
                    .ok_or(Unavailable::Bandwidth)
            });
            let (session, granted_bytes) = match result {
                Ok(Some((session, bytes, ()))) => (session, Some(bytes)),
                Err(Unavailable::Writer(session)) => {
                    self.scheduler.withdraw(session);
                    (session, None)
                }
                Ok(None) | Err(Unavailable::Bandwidth | Unavailable::Handoff) => break,
            };
            let id = self
                .sessions
                .get_mut(&session)
                .expect("ready session")
                .directions
                .pop_front()
                .expect("ready direction");
            let lane = self.lanes.get_mut(&id).expect("ready lane");
            lane.queued = false;
            if let Some(bytes) = granted_bytes {
                lane.heads.pop_front();
                lane.unclaimed += 1;
                let writer = self
                    .writers
                    .get_mut(&lane.writer)
                    .expect("live destination");
                writer.grant(id, bytes, now);
                if caller != Some(lane.writer) {
                    writer.wake.notify_one();
                }
                // Preserve the successor's service position before selecting again.
                self.enqueue(id, now);
            }
            self.publish_head(session, access);
        }
        self.pacing.charge(&available, &remaining);
    }

    pub(super) fn take(&mut self, writer: WriterId) -> Option<(u64, WritePermit)> {
        let destination = self.writers.get_mut(&writer)?;
        let (id, permit) = destination.take()?;
        let lane = self.lanes.get_mut(&id).expect("granted lane");
        lane.unclaimed -= 1;
        if destination.residents == 1 && lane.unclaimed == 0 && lane.heads.is_empty() {
            destination.window.scheduled.store(false, Ordering::Relaxed);
        }
        Some((id, permit))
    }

    pub(super) const fn waiting(&self) -> bool {
        self.scheduler.pending() != 0
    }

    pub(super) fn reset(&mut self) {
        self.scheduler = Scheduler::new(MAX_SESSIONS, MAX_FRAME);
        for session in self.sessions.values_mut() {
            session.directions.clear();
        }
        for lane in self.lanes.values_mut() {
            lane.heads.clear();
            lane.unclaimed = 0;
            lane.queued = false;
        }
        for destination in self.writers.values_mut() {
            destination.reset();
        }
    }

    pub(super) fn remove(&mut self, id: u64, access: &Service) {
        let lane = self.lanes.remove(&id).expect("live lane");
        let destination = self
            .writers
            .get_mut(&lane.writer)
            .expect("live destination");
        destination.discard(id);
        destination.residents -= 1;
        if destination.residents == 0 {
            destination.window.scheduled.store(false, Ordering::Relaxed);
            self.writers.remove(&lane.writer);
        }
        let session = self.sessions.get_mut(&lane.session).expect("live session");
        session.residents -= 1;
        if session.residents == 0 {
            self.sessions.remove(&lane.session);
            self.scheduler.cancel(lane.session);
        } else if lane.queued {
            let was_head = session.directions.front() == Some(&id);
            session.directions.retain(|other| *other != id);
            if was_head {
                self.scheduler.withdraw(lane.session);
                self.publish_head(lane.session, access);
            }
        }
    }
}
