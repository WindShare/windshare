use crate::{
    actor::{Actor, Event},
    config::{
        CONNECTION_ATTEMPTS, Endpoint, MAILBOX_CAPACITY, MAX_BUSINESS_CONNECTIONS,
        MAX_CONTROL_CONNECTIONS, MAX_RESIDENT_SHARES,
    },
    connection::Capacity,
    environment::{Environment, NativeEnvironment, incarnation},
    generation::{Generation, Recovery},
    socket,
    storage::Database,
};
use anyhow::{Result, anyhow, ensure};
use std::{
    collections::BTreeMap,
    future::Future,
    num::NonZeroU64,
    path::Path,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
};
use tokio::{
    net::TcpListener,
    sync::{mpsc, watch},
    task::{AbortHandle, JoinSet},
};
use windshare_relay_core::{
    identifiers::ShareId,
    lifecycle::{ConnectionRef, Record},
    share::Identity,
};

pub struct Handle {
    pub tx: mpsc::Sender<Event>,
    pub generation: Generation,
    idle: Arc<AtomicBool>,
    task: AbortHandle,
}

impl Drop for Handle {
    fn drop(&mut self) {
        self.retire();
    }
}

impl Handle {
    fn retire(&self) {
        self.generation.retire();
        self.task.abort();
    }
}

pub struct Registry {
    actors: Mutex<BTreeMap<ShareId, Arc<Handle>>>,
    store: Database,
    endpoint: Endpoint,
    environment: NativeEnvironment,
}

impl Registry {
    pub fn attach(&self, share: ShareId) -> Result<Arc<Handle>> {
        let mut actors = self
            .actors
            .lock()
            .map_err(|_| anyhow!("registry poisoned"))?;
        let recovery = if let Some(handle) = actors.get(&share) {
            if !handle.tx.is_closed() {
                return Ok(Arc::clone(handle));
            }
            handle.generation.recovery().inspect_err(|error| {
                tracing::debug!(event = "share_rebuild_deferred", ?share,
                    incarnation = ?handle.generation.identity().incarnation, %error);
            })?
        } else {
            Recovery::vacant(&self.store)
        };
        // A socket holds a strong lease even before its Connected event arrives.
        // Eviction must cross the same cleanup barrier as failure. Retaining its
        // slot prevents a vacant lookup from bypassing an unfinished generation.
        actors.retain(|_, handle| {
            if handle.generation.released() {
                return false;
            }
            if Arc::strong_count(handle) == 1 && handle.idle.load(Ordering::Acquire) {
                handle.retire();
            }
            true
        });
        ensure!(
            actors.len() < MAX_RESIDENT_SHARES,
            "resident share capacity exhausted"
        );
        let identity = Identity {
            relay: self.endpoint.identity,
            share,
            incarnation: incarnation()?,
        };
        let (tx, inbox) = mpsc::channel(MAILBOX_CAPACITY);
        let idle = Arc::new(AtomicBool::new(false));
        let store = self.store.clone();
        let environment = self.environment.clone();
        let activity = Arc::clone(&idle);
        let generation = Generation::new(identity, self.store.clone());
        let task = tokio::spawn(generation.track(Actor::start(
            store,
            environment,
            inbox,
            activity,
            generation.clone(),
            recovery,
        )));
        let task = generation.supervise(task);
        let handle = Arc::new(Handle {
            tx,
            generation,
            idle,
            task,
        });
        actors.insert(share, Arc::clone(&handle));
        drop(actors);
        Ok(handle)
    }

    async fn shutdown(&self) -> Result<()> {
        let actors = std::mem::take(
            &mut *self
                .actors
                .lock()
                .map_err(|_| anyhow!("registry poisoned"))?,
        );
        for handle in actors.values() {
            handle.retire();
        }
        for handle in actors.values() {
            handle.generation.wait_released().await;
        }
        Ok(())
    }
}

/// Owns one native listener's authority and bounded share actors.
pub struct Server {
    registry: Arc<Registry>,
}

impl Server {
    /// # Errors
    /// Refuses unknown authority, incompatible storage or too many recovering shares.
    pub fn open(endpoint: Endpoint, database: &Path) -> Result<Self> {
        let store = Database::open(database, endpoint.identity)?;
        let records = store.recoverable_records(MAX_RESIDENT_SHARES)?;
        let registry = Arc::new(Registry {
            actors: Mutex::new(BTreeMap::new()),
            store,
            endpoint,
            environment: NativeEnvironment::new()?,
        });
        // Lost-owner grace starts at process recovery, not at the next request.
        // Permanent STOP facts stay on disk without retaining an idle actor each.
        for record in records {
            if !matches!(record, Record::Stopped(_)) {
                registry.attach(record.share())?;
            }
        }
        Ok(Self { registry })
    }

    /// Runs until shutdown, isolating each socket's waits from share decisions.
    ///
    /// # Errors
    /// Returns listener errors or connection-identity exhaustion.
    pub async fn serve(
        self,
        listener: TcpListener,
        shutdown: impl Future<Output = ()>,
    ) -> Result<()> {
        let slots = Capacity::new(
            MAX_BUSINESS_CONNECTIONS,
            MAX_CONTROL_CONNECTIONS,
            CONNECTION_ATTEMPTS,
            self.registry.environment.now(),
        )?;
        let mut tasks = JoinSet::new();
        let (stop_connections, stopping) = watch::channel(false);
        let mut next_connection = NonZeroU64::MIN;
        tokio::pin!(shutdown);
        let result = loop {
            tokio::select! {
                () = &mut shutdown => break Ok(()),
                result = tasks.join_next(), if !tasks.is_empty() => {
                    if let Some(Err(error)) = result {
                        tracing::error!(event = "socket_task_failed", %error);
                    }
                }
                accepted = listener.accept() => {
                    let (stream, peer) = match accepted { Ok(value) => value, Err(error) => break Err(error.into()) };
                    let admission = match slots.accept(self.registry.environment.now()) {
                        Ok(admission) => admission,
                        Err(error) => {
                            tracing::warn!(event = "control_connection_refused", %peer, %error);
                            continue;
                        }
                    };
                    let id = next_connection;
                    let Some(next) = next_connection.checked_add(1) else {
                        break Err(anyhow!("connection identity exhausted"));
                    };
                    next_connection = next;
                    let connection = ConnectionRef {
                        id,
                        generation: self.registry.store.generation(),
                    };
                    let endpoint = self.registry.endpoint.clone();
                    let registry = Arc::clone(&self.registry);
                    let stopping = stopping.clone();
                    tasks.spawn(async move {
                        if let Err(error) = socket::serve(stream, endpoint, registry, connection, stopping, admission).await {
                            tracing::debug!(event = "socket_ended", ?connection, %peer, %error);
                        }
                    });
                }
            }
        };
        // Supervisors must join their own reader/writer tasks and return release
        // receipts before actors disappear. Aborting supervisors would skip that work.
        stop_connections.send_replace(true);
        while tasks.join_next().await.is_some() {}
        self.registry.shutdown().await?;
        result
    }
}

#[cfg(test)]
#[path = "registry_tests.rs"]
mod tests;
