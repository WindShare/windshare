use crate::config::{SUBMITTED_DATA_BYTES, SUBMITTED_DATA_FRAMES};
use std::sync::Arc;
use tokio::sync::{OwnedSemaphorePermit, Semaphore};

/// All flows on a socket contend for the same physical writer capacity. Its
/// permits follow the frame from bandwidth allocation through socket completion.
pub struct WriteWindow {
    pub(super) frames: Arc<Semaphore>,
    pub(super) bytes: Arc<Semaphore>,
}

pub struct WritePermit {
    pub(super) slot: OwnedSemaphorePermit,
    pub(super) bytes: OwnedSemaphorePermit,
}

impl Default for WriteWindow {
    fn default() -> Self {
        Self {
            frames: Arc::new(Semaphore::new(SUBMITTED_DATA_FRAMES)),
            bytes: Arc::new(Semaphore::new(SUBMITTED_DATA_BYTES)),
        }
    }
}

impl WriteWindow {
    pub(crate) fn fits(&self, bytes: u64) -> bool {
        self.frames.available_permits() > 0 && bytes <= self.bytes.available_permits() as u64
    }

    pub(crate) fn reserve(&self, bytes: u64) -> Option<WritePermit> {
        Some(WritePermit {
            slot: Arc::clone(&self.frames).try_acquire_owned().ok()?,
            bytes: Arc::clone(&self.bytes)
                .try_acquire_many_owned(u32::try_from(bytes).ok()?)
                .ok()?,
        })
    }
}
