use crate::{actor::Event, storage::Database};
use anyhow::{Result, ensure};
use std::{
    future::Future,
    num::NonZeroU64,
    pin::Pin,
    sync::Arc,
    task::{Context, Poll},
};
use tokio::{
    sync::{mpsc, watch},
    task::{AbortHandle, JoinHandle},
};
use tokio_util::{sync::CancellationToken, task::TaskTracker};
use windshare_relay_core::{
    lifecycle::Record,
    share::{Identity, Responsibility, Restore},
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Phase {
    Running,
    Draining,
    Released,
}

struct State {
    identity: Identity,
    tasks: TaskTracker,
    phase: watch::Sender<Phase>,
    stopping: CancellationToken,
    // A new host cannot acquire the authority lease while an orphaned task still
    // owns native resources, even if its actor, registry or server was dropped.
    _store: Database,
}

#[derive(Clone)]
pub struct Generation(Arc<State>);

/// Evidence is issued by the host's empty inventory or a completed cleanup barrier.
pub struct Recovery(Evidence);

enum Evidence {
    Vacant { host_generation: NonZeroU64 },
    Drained { predecessor: Identity },
}

impl Recovery {
    /// The registry must retain every unfinished generation, including idle eviction.
    pub fn vacant(store: &Database) -> Self {
        Self(Evidence::Vacant {
            host_generation: store.generation(),
        })
    }

    pub fn restore(self, identity: Identity, record: Option<Record>) -> Result<Restore<'static>> {
        match self.0 {
            Evidence::Vacant { host_generation } => {
                // Native transports use abortive close and never export buffer
                // ownership. The exclusive host lease, not a restart timestamp,
                // fences prior process resources; the registry fences local ones.
                let cause = if record.is_none() {
                    "fresh_authority"
                } else {
                    "host_recovery"
                };
                tracing::debug!(event = "delivery_recovery_released", share = ?identity.share,
                    incarnation = ?identity.incarnation, %host_generation, cause);
            }
            Evidence::Drained { predecessor } => {
                ensure!(
                    predecessor.share == identity.share,
                    "release belongs to another share"
                );
                ensure!(
                    predecessor.incarnation != identity.incarnation,
                    "actor incarnation reused"
                );
                tracing::debug!(event = "delivery_recovery_released", share = ?identity.share,
                    incarnation = ?identity.incarnation, previous = ?predecessor.incarnation,
                    cause = "actor_cleanup");
            }
        }
        Ok(Restore::restarted(record, Responsibility::Released))
    }
}

impl Generation {
    pub fn new(identity: Identity, store: Database) -> Self {
        Self(Arc::new(State {
            identity,
            tasks: TaskTracker::new(),
            phase: watch::channel(Phase::Running).0,
            stopping: CancellationToken::new(),
            _store: store,
        }))
    }

    pub fn identity(&self) -> Identity {
        self.0.identity
    }

    pub fn admit<F: Future>(
        &self,
        events: &mpsc::Sender<Event>,
        future: F,
    ) -> Result<impl Future<Output = F::Output> + use<F>> {
        // Registration and retirement share the watch lock: a stale handle cannot
        // add a task after the closed-and-empty barrier has completed.
        let phase = self.0.phase.borrow();
        ensure!(
            *phase == Phase::Running && !events.is_closed(),
            "share generation is retiring"
        );
        let tracked = self.track(future);
        drop(phase);
        Ok(tracked)
    }

    /// Only an already tracked actor/connection may create descendant work.
    pub fn track<F: Future>(&self, future: F) -> impl Future<Output = F::Output> + use<F> {
        self.0.tasks.track_future(Owned {
            future: Box::pin(future),
            _generation: self.clone(),
        })
    }

    pub fn spawn_blocking<F, T>(&self, task: F) -> JoinHandle<T>
    where
        F: FnOnce() -> T + Send + 'static,
        T: Send + 'static,
    {
        let generation = self.clone();
        self.0.tasks.spawn_blocking(move || {
            let result = task();
            drop(generation);
            result
        })
    }

    pub async fn cancelled(&self) {
        self.0.stopping.cancelled().await;
    }

    pub fn retire(&self) {
        if self.0.phase.send_if_modified(|phase| {
            if *phase != Phase::Running {
                return false;
            }
            *phase = Phase::Draining;
            true
        }) {
            tracing::debug!(event = "share_cleanup_started", share = ?self.0.identity.share,
                incarnation = ?self.0.identity.incarnation, tasks = self.0.tasks.len());
            self.0.stopping.cancel();
            self.0.tasks.close();
        }
    }

    pub fn released(&self) -> bool {
        *self.0.phase.borrow() == Phase::Released
    }

    pub fn recovery(&self) -> Result<Recovery> {
        ensure!(
            self.released(),
            "previous share generation is still releasing resources"
        );
        Ok(Recovery(Evidence::Drained {
            predecessor: self.0.identity,
        }))
    }

    pub async fn wait_released(&self) {
        let _ = self
            .0
            .phase
            .subscribe()
            .wait_for(|phase| *phase == Phase::Released)
            .await;
    }

    pub fn supervise(&self, actor: JoinHandle<Result<()>>) -> AbortHandle {
        let abort = actor.abort_handle();
        let generation = self.clone();
        // This monitor is independent of the actor and the server's connection
        // JoinSet. Actor panic/cancellation cannot cancel its cleanup obligation.
        tokio::spawn(async move {
            let result = actor.await;
            generation.retire();
            match result {
                Ok(Ok(())) => {}
                Err(error) if error.is_cancelled() => {}
                result => tracing::error!(event = "share_host_failed",
                    share = ?generation.0.identity.share,
                    incarnation = ?generation.0.identity.incarnation, ?result),
            }
            generation.0.tasks.wait().await;
            generation.0.phase.send_replace(Phase::Released);
            tracing::debug!(event = "share_cleanup_released", share = ?generation.0.identity.share,
                incarnation = ?generation.0.identity.incarnation);
        });
        abort
    }
}

// Field order keeps the host lease alive until the entire task future, including
// its captured transport and queued frames, has been destroyed on cancellation.
struct Owned<F> {
    future: Pin<Box<F>>,
    _generation: Generation,
}

impl<F: Future> Future for Owned<F> {
    type Output = F::Output;

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        self.future.as_mut().poll(cx)
    }
}

#[cfg(test)]
#[path = "generation_tests.rs"]
mod tests;
