use super::{Bandwidth, Key, MAX_FRAME, MAX_SESSIONS, Service, WritePermit, WriteWindow};
use crate::config::SUBMITTED_DATA_FRAMES;
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
}

struct Lane {
    session: Key,
    attribution: Attribution,
    window: Arc<WriteWindow>,
    wake: Arc<Notify>,
    heads: VecDeque<u64>,
    grants: VecDeque<(u128, WritePermit)>,
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
    grant_order: u128,
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
            grant_order: 0,
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
        self.sessions.entry(session).or_default().residents += 1;
        self.lanes.insert(
            id,
            Lane {
                session,
                attribution,
                window,
                wake,
                heads: VecDeque::new(),
                grants: VecDeque::new(),
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

    pub(super) fn offer(&mut self, id: u64, sizes: impl Iterator<Item = u64>, access: &Service) {
        let lane = self.lanes.get_mut(&id).expect("live lane");
        // Published frames stay in the actor FIFO until it takes their grants.
        let known = lane.grants.len() + lane.heads.len();
        lane.heads
            .extend(sizes.take(SUBMITTED_DATA_FRAMES).skip(known));
        let session = lane.session;
        if self.enqueue(id) {
            self.publish_head(session, access);
        }
    }

    /// Returns true when the session has gained its first competing direction.
    fn enqueue(&mut self, id: u64) -> bool {
        let lane = self.lanes.get_mut(&id).expect("live lane");
        if lane.queued
            || !lane
                .heads
                .front()
                .is_some_and(|&bytes| lane.window.fits(bytes))
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

    pub(super) fn grant_pending(&mut self, access: &Service, caller: Option<&WriteWindow>) {
        let available = self.pacing.replenish(Instant::now());
        let mut remaining = available;
        loop {
            let result = self.scheduler.try_next(|session, class, bytes| {
                let id = self.sessions[&session].directions[0];
                let window = &self.lanes[&id].window;
                // Another flow can fill the same writer after this head was
                // advertised. Charge neither tokens nor service tags if it did.
                let mut credit = remaining;
                if !credit.reserve(class, bytes) {
                    return Err(if window.fits(bytes) {
                        Unavailable::Bandwidth
                    } else {
                        Unavailable::Writer(session)
                    });
                }
                let permit = window.reserve(bytes).ok_or(Unavailable::Writer(session))?;
                remaining = credit;
                Ok(permit)
            });
            let (session, permit) = match result {
                Ok(Some((session, _, permit))) => (session, Some(permit)),
                Err(Unavailable::Writer(session)) => {
                    self.scheduler.withdraw(session);
                    (session, None)
                }
                Ok(None) | Err(Unavailable::Bandwidth) => break,
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
            if let Some(permit) = permit {
                lane.heads.pop_front();
                self.grant_order += 1;
                lane.grants.push_back((self.grant_order, permit));
                if !caller.is_some_and(|window| std::ptr::eq(window, lane.window.as_ref())) {
                    lane.wake.notify_one();
                }
                // Preserve the successor's service position before selecting again.
                self.enqueue(id);
            }
            self.publish_head(session, access);
        }
        self.pacing.charge(&available, &remaining);
    }

    pub(super) fn take_first(
        &mut self,
        lanes: impl Iterator<Item = u64>,
    ) -> Option<(usize, WritePermit)> {
        let (index, id, _) = lanes
            .enumerate()
            .filter_map(|(index, id)| {
                let (order, _) = self.lanes[&id].grants.front()?;
                Some((index, id, *order))
            })
            .min_by_key(|entry| entry.2)?;
        Some((index, self.lanes.get_mut(&id)?.grants.pop_front()?.1))
    }

    pub(super) fn remove(&mut self, id: u64, access: &Service) {
        let lane = self.lanes.remove(&id).expect("live lane");
        if !lane.grants.is_empty() {
            // Discarding an unsent grant opens a writer slot without a socket completion.
            lane.wake.notify_one();
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
