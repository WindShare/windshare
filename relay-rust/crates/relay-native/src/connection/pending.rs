use super::write_window::{WritePermit, WriteWindow};
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
    wake: Arc<Notify>,
    pub(super) window: Arc<WriteWindow>,
    flows: Vec<Flow>,
    frames: usize,
}

impl Queues {
    pub fn set_wake(&mut self, wake: Arc<Notify>) {
        self.wake = wake;
    }

    pub const fn len(&self) -> usize {
        self.frames
    }
    pub const fn is_empty(&self) -> bool {
        self.frames == 0
    }

    pub fn push(
        &mut self,
        pending: Pending,
        governor: &Governor,
        attribution: Attribution,
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
                .register(
                    key,
                    attribution,
                    Arc::clone(&self.wake),
                    Arc::clone(&self.window),
                )
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

    pub fn take(&mut self, governor: &Governor) -> Option<(Pending, WritePermit)> {
        let (index, permit) = governor.take(
            &self.window,
            self.flows.iter().map(|flow| {
                (
                    &flow.scheduling,
                    flow.queue.iter().map(|frame| frame.bytes.len() as u64),
                )
            }),
        )?;
        let flow = &mut self.flows[index];
        let head = flow.queue.pop_front().expect("scheduled head");
        self.frames -= 1;
        Some((head, permit))
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
