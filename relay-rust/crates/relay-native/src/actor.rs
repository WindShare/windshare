use crate::{
    config::limits,
    connection::Outbox,
    environment::{Environment, Trace},
    generation::{Generation, Recovery},
    storage::Persistence,
};
use anyhow::{Result, anyhow};
use std::{
    collections::BTreeMap,
    future::pending,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};
use tokio::{
    sync::{mpsc, oneshot},
    task::JoinHandle,
    time::Instant,
};
use windshare_relay_core::{
    authority::StoredAuthority,
    lifecycle::ConnectionRef,
    share::{Identity, Input, ShareRelay, WorkId, WorkResult},
};

mod ingress;
mod outputs;
mod windows;
use windows::Windows;

pub enum Event {
    Connected(ConnectionRef, Outbox),
    Control(ConnectionRef, Vec<u8>),
    Frame(ConnectionRef, Vec<u8>),
    Unsupported(ConnectionRef),
    Writable(ConnectionRef),
    Disconnected(ConnectionRef),
    Closed(ConnectionRef, oneshot::Sender<()>),
}

type Committed = (WorkId, Result<Option<StoredAuthority>>);
type Commit = JoinHandle<Committed>;

pub struct Actor<S, E> {
    relay: ShareRelay<Trace>,
    identity: Identity,
    store: S,
    authority: Option<StoredAuthority>,
    environment: E,
    ports: BTreeMap<ConnectionRef, Outbox>,
    windows: Windows,
    inbox: mpsc::Receiver<Event>,
    commit: Option<Commit>,
    retry: Option<(Instant, WorkId)>,
    idle: Arc<AtomicBool>,
    generation: Generation,
}

impl<S: Persistence, E: Environment> Actor<S, E> {
    pub async fn start(
        store: S,
        environment: E,
        inbox: mpsc::Receiver<Event>,
        idle: Arc<AtomicBool>,
        generation: Generation,
        recovery: Recovery,
    ) -> Result<()> {
        let identity = generation.identity();
        let reader = store.clone();
        let authority = generation
            .spawn_blocking(move || reader.load(identity.share))
            .await??;
        let restored =
            recovery.restore(identity, authority.as_ref().map(StoredAuthority::record))?;
        let (relay, initial) =
            ShareRelay::restore(identity, limits(), &restored, Trace, environment.now())
                .map_err(|error| anyhow!("restore refused: {error:?}"))?;
        let mut actor = Self {
            relay,
            identity,
            store,
            authority,
            environment,
            ports: BTreeMap::new(),
            windows: Windows::default(),
            inbox,
            commit: None,
            retry: None,
            idle,
            generation,
        };
        actor.outputs(initial)?;
        actor.run().await
    }

    async fn run(&mut self) -> Result<()> {
        loop {
            self.idle.store(
                self.ports.is_empty()
                    && self.commit.is_none()
                    && self.retry.is_none()
                    && self.relay.deadline().is_none()
                    && self.inbox.is_empty(),
                Ordering::Release,
            );
            let wake = self.wake();
            let turn = tokio::select! {
                event = self.inbox.recv() => {
                    let Some(event) = event else { return Ok(()); };
                    self.event(event)?;
                    continue;
                }
                result = complete(&mut self.commit) => {
                    self.commit = None;
                    let (id, result) = result?;
                    let outcome = match result {
                        Ok(authority) => {
                            // A JOIN can use descriptor bytes only from a confirmed
                            // transaction, including after a lost receipt is retried.
                            self.authority = authority;
                            WorkResult::Complete
                        }
                        Err(error) => {
                            tracing::error!(event = "storage_uncertain", share = ?self.identity.share,
                                incarnation = ?id.incarnation(), work = id.sequence(), %error);
                            WorkResult::Uncertain
                        }
                    };
                    self.relay.handle(Input::Completed { id, result: outcome }, self.environment.now())
                }
                () = wait(wake) => {
                    let input = if self.retry.is_some_and(|(at, _)| at <= Instant::now()) {
                        Input::Retry(self.retry.take().expect("due retry").1)
                    } else { Input::Tick };
                    self.relay.handle(input, self.environment.now())
                }
            };
            self.outputs(turn)?;
        }
    }

    fn wake(&self) -> Option<Instant> {
        let kernel = self.relay.deadline().map(|deadline| {
            Instant::now()
                + Duration::from_millis(deadline.0.saturating_sub(self.environment.now().0))
        });
        match (kernel, self.retry.map(|(at, _)| at)) {
            (Some(a), Some(b)) => Some(a.min(b)),
            (value, None) | (None, value) => value,
        }
    }
}

async fn complete(commit: &mut Option<Commit>) -> Result<Committed> {
    match commit {
        Some(task) => Ok(task.await?),
        None => pending().await,
    }
}

async fn wait(at: Option<Instant>) {
    match at {
        Some(at) => tokio::time::sleep_until(at).await,
        None => pending().await,
    }
}

#[cfg(test)]
#[path = "actor_tests.rs"]
mod tests;
