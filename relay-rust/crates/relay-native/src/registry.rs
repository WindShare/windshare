use crate::{
    actor::{Actor, Event},
    config::{Endpoint, MAILBOX_CAPACITY, MAX_CONNECTIONS, MAX_RESIDENT_SHARES},
    environment::{NativeEnvironment, incarnation},
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
    sync::{Semaphore, mpsc},
    task::{AbortHandle, JoinSet},
};
use windshare_relay_core::{
    identifiers::ShareId,
    lifecycle::{ConnectionRef, Record},
    share::Identity,
};

pub struct Handle {
    pub tx: mpsc::Sender<Event>,
    idle: Arc<AtomicBool>,
    task: AbortHandle,
}

impl Drop for Handle {
    fn drop(&mut self) {
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
        if let Some(handle) = actors.get(&share)
            && !handle.tx.is_closed()
        {
            return Ok(Arc::clone(handle));
        }
        // A socket holds a strong lease even before its Connected event arrives.
        // Only deadline-free actors with no socket leases can be evicted.
        actors.retain(|_, handle| {
            !(handle.tx.is_closed()
                || Arc::strong_count(handle) == 1 && handle.idle.load(Ordering::Acquire))
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
        let task = tokio::spawn(async move {
            if let Err(error) = Actor::start(identity, store, environment, inbox, activity).await {
                tracing::error!(event = "share_host_failed", ?share, incarnation = ?identity.incarnation, %error);
            }
        });
        let handle = Arc::new(Handle {
            tx,
            idle,
            task: task.abort_handle(),
        });
        actors.insert(share, Arc::clone(&handle));
        drop(actors);
        Ok(handle)
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
        let slots = Arc::new(Semaphore::new(MAX_CONNECTIONS));
        let mut tasks = JoinSet::new();
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
                    let Ok(slot) = Arc::clone(&slots).try_acquire_owned() else {
                        tracing::warn!(event = "connection_capacity", %peer);
                        continue;
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
                    let registry = Arc::clone(&self.registry);
                    let endpoint = registry.endpoint.clone();
                    tasks.spawn(async move {
                        let _slot = slot;
                        if let Err(error) = socket::serve(stream, endpoint, registry, connection).await {
                            tracing::debug!(event = "socket_ended", ?connection, %peer, %error);
                        }
                    });
                }
            }
        };
        tasks.abort_all();
        while tasks.join_next().await.is_some() {}
        self.registry
            .actors
            .lock()
            .map_err(|_| anyhow!("registry poisoned"))?
            .clear();
        result
    }
}
