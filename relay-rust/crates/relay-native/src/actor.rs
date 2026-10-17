use crate::{
    config::{STORAGE_RETRY, limits},
    environment::{Environment, Trace},
    storage::Persistence,
};
use anyhow::{Result, anyhow, bail, ensure};
use std::{
    collections::{BTreeMap, VecDeque},
    future::pending,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};
use tokio::{sync::mpsc, task::JoinHandle, time::Instant};
use windshare_relay_core::{
    lifecycle::ConnectionRef,
    share::{
        Control, Identity, Input, Output, Responsibility, Restore, ShareRelay, Turn, WorkId,
        WorkResult,
    },
};

pub enum Event {
    Connected(ConnectionRef, mpsc::Sender<Vec<u8>>),
    Control(ConnectionRef, Vec<u8>),
    Closed(ConnectionRef),
}

type Commit = JoinHandle<(WorkId, Result<()>)>;

pub struct Actor<S, E> {
    relay: ShareRelay<Trace>,
    identity: Identity,
    store: S,
    environment: E,
    ports: BTreeMap<ConnectionRef, mpsc::Sender<Vec<u8>>>,
    inbox: mpsc::Receiver<Event>,
    commit: Option<Commit>,
    retry: Option<(Instant, WorkId)>,
    idle: Arc<AtomicBool>,
}

impl<S: Persistence, E: Environment> Actor<S, E> {
    pub async fn start(
        identity: Identity,
        store: S,
        environment: E,
        inbox: mpsc::Receiver<Event>,
        idle: Arc<AtomicBool>,
    ) -> Result<()> {
        let reader = store.clone();
        let record = tokio::task::spawn_blocking(move || reader.load(identity.share)).await??;
        // This schema and ingress never admit JOIN or opaque payloads. Therefore
        // no generation has ever acquired delivery debt; restart alone is not proof.
        let restored = Restore::restarted(record, Responsibility::Released);
        let (relay, initial) =
            ShareRelay::restore(identity, limits(), &restored, Trace, environment.now())
                .map_err(|error| anyhow!("restore refused: {error:?}"))?;
        let mut actor = Self {
            relay,
            identity,
            store,
            environment,
            ports: BTreeMap::new(),
            inbox,
            commit: None,
            retry: None,
            idle,
        };
        actor.outputs(initial)?;
        let result = actor.run().await;
        // A failed actor cannot be replaced while one of its writes might still
        // commit. Keep its mailbox alive until the blocking obligation quiesces.
        if let Some(commit) = actor.commit.take() {
            let (_, completion) = commit.await?;
            if let Err(error) = completion {
                tracing::error!(event = "shutdown_storage_uncertain", %error);
            }
        }
        result
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
                    self.event(event)
                }
                result = complete(&mut self.commit) => {
                    self.commit = None;
                    let (id, result) = result?;
                    let outcome = match result {
                        Ok(()) => WorkResult::Complete,
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

    fn event(&mut self, event: Event) -> Turn {
        let now = self.environment.now();
        match event {
            Event::Connected(connection, writer) => {
                self.ports.insert(connection, writer);
                self.relay.handle(Input::Connected(connection), now)
            }
            Event::Control(connection, frame) => {
                if !self.ports.contains_key(&connection) {
                    return Turn::default();
                }
                let mut turn = self.relay.handle(
                    Input::Control {
                        connection,
                        frame: &frame,
                    },
                    now,
                );
                if turn.refusal.is_some() {
                    self.ports.remove(&connection);
                    turn.outputs.extend(
                        self.relay
                            .handle(Input::Disconnected(connection), now)
                            .outputs,
                    );
                }
                turn
            }
            Event::Closed(connection) => {
                self.ports.remove(&connection);
                // The connection task has dropped its writer and queues. No data
                // was submitted in this lifecycle-only host, so no debt is refunded.
                self.relay.handle(Input::Closed(connection), now)
            }
        }
    }

    fn outputs(&mut self, turn: Turn) -> Result<()> {
        let mut outputs: VecDeque<_> = turn.outputs.into();
        while let Some(output) = outputs.pop_front() {
            let follow = match output {
                Output::ChallengeNeeded(request) => Some(self.relay.handle(
                    Input::Challenge {
                        request,
                        seed: self.environment.challenge()?,
                    },
                    self.environment.now(),
                )),
                Output::Send {
                    connection,
                    control,
                } => {
                    let bytes = encode(control)?;
                    let sent = self
                        .ports
                        .get(&connection)
                        .is_some_and(|writer| writer.try_send(bytes).is_ok());
                    if sent {
                        None
                    } else {
                        tracing::warn!(event = "control_writer_unavailable", share = ?self.identity.share, ?connection);
                        self.ports.remove(&connection);
                        Some(
                            self.relay
                                .handle(Input::Disconnected(connection), self.environment.now()),
                        )
                    }
                }
                Output::Close(connection) => {
                    // Dropping the sender drains already-ordered responses before
                    // transport closure, including STOPPED immediately before Close.
                    self.ports.remove(&connection);
                    None
                }
                Output::Work(work) => {
                    ensure!(
                        self.commit.is_none(),
                        "kernel issued concurrent storage obligations"
                    );
                    let store = self.store.clone();
                    let share = self.identity.share;
                    tracing::debug!(event = "storage_started", ?share, work = work.id.sequence(),
                        incarnation = ?work.id.incarnation());
                    self.commit = Some(tokio::task::spawn_blocking(move || {
                        let result = store.execute(share, &work.operation);
                        (work.id, result)
                    }));
                    None
                }
                Output::Retry(id) => {
                    self.retry = Some((Instant::now() + STORAGE_RETRY, id));
                    None
                }
                Output::Discard(flow) => Some(
                    self.relay
                        .handle(Input::Discarded(flow), self.environment.now()),
                ),
                Output::Received(_) => bail!("payload reached lifecycle-only host"),
                Output::Reload => bail!("kernel requires authoritative reload"),
            };
            if let Some(turn) = follow {
                outputs.extend(turn.outputs);
            }
        }
        Ok(())
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

async fn complete(commit: &mut Option<Commit>) -> Result<(WorkId, Result<()>)> {
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

fn encode(control: Control) -> Result<Vec<u8>> {
    Ok(match control {
        Control::Challenge(bytes) => bytes,
        Control::Error(error) => error.encode().to_vec(),
        Control::Registered(frame) => frame.encode().to_vec(),
        Control::Stopped(frame) => frame.encode().to_vec(),
        Control::ProbeAck(frame) => frame.encode().to_vec(),
        Control::Descriptor { .. } | Control::Grant(_) | Control::Settlement(_) => {
            bail!("delivery command reached lifecycle-only host")
        }
    })
}

#[cfg(test)]
#[path = "actor_tests.rs"]
mod tests;
