use crate::node::governor::{Governor, LaneLease};
use anyhow::{Result, anyhow, ensure};
use std::{collections::VecDeque, sync::Arc, time::Duration};
use tokio::time::Instant;

const PRESSURE_THRESHOLD: Duration = Duration::from_secs(1);
const PRESSURE_REFRESH: Duration = Duration::from_secs(5);
const PRESSURE_LEASE_MS: u32 = 15_000;
use tokio::sync::Notify;
use windshare_relay_access::Attribution;
use windshare_relay_core::flow::{FlowKey, ReceivedFrame};

pub(super) struct Pending {
    pub receipt: ReceivedFrame,
    pub bytes: Box<[u8]>,
    pub queued_at: Instant,
}

struct Flow {
    key: FlowKey,
    queue: VecDeque<Pending>,
    scheduling: LaneLease,
    pressure_reported: Option<Instant>,
}

#[derive(Default)]
pub(super) struct Queues {
    flows: Vec<Flow>,
    frames: usize,
    cursor: usize,
}

impl Queues {
    pub const fn len(&self) -> usize {
        self.frames
    }
    #[cfg(test)]
    pub const fn is_empty(&self) -> bool {
        self.frames == 0
    }

    pub fn push(
        &mut self,
        pending: Pending,
        governor: &Governor,
        attribution: Attribution,
        wake: &Arc<Notify>,
    ) -> Result<()> {
        let key = pending.receipt.flow();
        let index = self.flows.iter().position(|flow| flow.key == key);
        let flow = if let Some(index) = index {
            &mut self.flows[index]
        } else {
            ensure!(
                self.flows.len() < crate::config::DATA_QUEUE_FRAMES,
                "session queue limit"
            );
            let scheduling = governor
                .register(key, attribution, Arc::clone(wake))
                .ok_or_else(|| anyhow!("node session scheduling capacity"))?;
            self.flows.push(Flow {
                key,
                queue: VecDeque::new(),
                scheduling,
                pressure_reported: None,
            });
            self.flows.last_mut().expect("inserted flow")
        };
        ensure!(
            flow.queue.len() < crate::config::DATA_QUEUE_FRAMES,
            "session pending frame limit"
        );
        flow.queue.push_back(pending);
        self.frames += 1;
        Ok(())
    }

    pub fn next(&self) -> Option<(usize, usize)> {
        for offset in 0..self.flows.len() {
            let index = (self.cursor + offset) % self.flows.len();
            let flow = &self.flows[index];
            let Some(head) = flow.queue.front() else {
                continue;
            };
            if flow.scheduling.ready(head.bytes.len() as u64) {
                return Some((index, head.bytes.len()));
            }
        }
        None
    }

    pub fn pop(&mut self, index: usize) -> Pending {
        let flow = &mut self.flows[index];
        let head = flow.queue.pop_front().expect("scheduled head");
        flow.scheduling.submitted();
        self.frames -= 1;
        self.cursor = (index + 1) % self.flows.len();
        head
    }

    pub fn discard(&mut self, key: FlowKey) -> usize {
        let Some(index) = self.flows.iter().position(|flow| flow.key == key) else {
            return 0;
        };
        let flow = self.flows.remove(index);
        self.frames -= flow.queue.len();
        flow.queue.iter().map(|pending| pending.bytes.len()).sum()
    }

    pub fn capacity_updates(&mut self, now: Instant) -> Vec<(FlowKey, u32)> {
        let mut updates = Vec::new();
        for flow in &mut self.flows {
            let waiting = flow
                .queue
                .front()
                .is_some_and(|head| now.duration_since(head.queued_at) >= PRESSURE_THRESHOLD);
            if waiting {
                if flow
                    .pressure_reported
                    .is_none_or(|last| now.duration_since(last) >= PRESSURE_REFRESH)
                {
                    flow.pressure_reported = Some(now);
                    updates.push((flow.key, PRESSURE_LEASE_MS));
                }
            } else if flow.pressure_reported.take().is_some() {
                updates.push((flow.key, 0));
            }
        }
        updates
    }

    pub fn clear(&mut self) {
        self.flows.clear();
        self.frames = 0;
    }
}
