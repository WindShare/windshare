use super::{Actor, Environment, Persistence};
use crate::config::STORAGE_RETRY;
use anyhow::{Result, anyhow, bail, ensure};
use std::collections::VecDeque;
use tokio::time::Instant;
use windshare_relay_core::{
    flow::Counts,
    lifecycle::ConnectionRef,
    sessions::encode_descriptor,
    share::{Control, Input, Output, Turn, Work},
};

impl<S: Persistence, E: Environment> Actor<S, E> {
    pub(super) fn outputs(&mut self, turn: Turn) -> Result<()> {
        let mut outputs: VecDeque<_> = turn.outputs.into();
        loop {
            while let Some(output) = outputs.pop_front() {
                if let Some(follow) = self.output(output)? {
                    // Evidence completes this command before the next one runs:
                    // discard must enqueue its ordered settlement before Close.
                    for output in follow.outputs.into_iter().rev() {
                        outputs.push_front(output);
                    }
                }
            }
            let Some((flow, credit)) = self.windows.next(&self.relay, |source| {
                self.ports
                    .get(&source)
                    .is_some_and(crate::connection::Outbox::control_available)
            }) else {
                return Ok(());
            };
            let now = self.environment.now();
            let turn = self.relay.handle(Input::Grant { flow, credit }, now);
            outputs.extend(turn.outputs);
            if let Some(error) = turn.refusal {
                // An offer is advisory. Expiry or counter exhaustion retires only
                // this admission; its shared sender and siblings must stay live.
                self.windows.remove(flow);
                tracing::debug!(event = "delivery_window_refused", share = ?self.identity.share, ?flow, ?error);
                outputs.extend(
                    self.relay
                        .handle(
                            Input::Cancel {
                                participant: flow.permit().source(),
                                session: flow.session().wire_id(),
                            },
                            now,
                        )
                        .outputs,
                );
            }
        }
    }

    fn output(&mut self, output: Output) -> Result<Option<Turn>> {
        Ok(match output {
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
            } => self.send(connection, control)?,
            Output::Close(connection) => {
                // Only an explicit kernel close drains ordered replies. Failure,
                // overflow and actor teardown abort instead of retaining writers.
                if let Some(port) = self.ports.remove(&connection) {
                    port.finish();
                }
                None
            }
            Output::Work(work) => {
                self.persist(work)?;
                None
            }
            Output::Retry(id) => {
                self.retry = Some((Instant::now() + STORAGE_RETRY, id));
                None
            }
            Output::Discard(flow) => {
                self.windows.remove(flow);
                if let Some(port) = self.ports.get_mut(&flow.destination()) {
                    port.discard(flow);
                }
                // A missing port has already dropped its entire pending queue.
                tracing::debug!(event = "flow_queue_discarded", share = ?self.identity.share, ?flow);
                Some(
                    self.relay
                        .handle(Input::Discarded(flow), self.environment.now()),
                )
            }
            Output::Received(_) => bail!("received receipt without its ingress payload"),
            Output::Reload => bail!("kernel requires authoritative reload"),
        })
    }

    fn send(&mut self, connection: ConnectionRef, control: Control) -> Result<Option<Turn>> {
        let probe = matches!(control, Control::ProbeAck(_));
        let flow = if let Control::Grant(grant) = &control {
            if self
                .relay
                .delivery_window(grant.flow, Counts::ZERO)
                .is_none()
            {
                return Ok(None);
            }
            tracing::trace!(event = "delivery_credit_issued", share = ?self.identity.share,
                ?connection, flow = ?grant.flow, credit = ?grant.credit);
            Some(grant.flow)
        } else {
            None
        };
        let bytes = self.encode(control)?;
        let sent = self.ports.get(&connection).is_some_and(|writer| {
            if probe {
                writer.probe(bytes)
            } else {
                writer.control(bytes)
            }
            .is_ok()
        });
        if sent {
            if let Some(flow) = flow {
                self.windows.track(flow);
            }
            Ok(None)
        } else {
            tracing::warn!(event = "control_writer_unavailable", share = ?self.identity.share, ?connection);
            self.ports.remove(&connection);
            Ok(Some(self.relay.handle(
                Input::Disconnected(connection),
                self.environment.now(),
            )))
        }
    }

    fn persist(&mut self, work: Work) -> Result<()> {
        ensure!(
            self.commit.is_none(),
            "kernel issued concurrent storage obligations"
        );
        let store = self.store.clone();
        let share = self.identity.share;
        tracing::debug!(event = "storage_started", ?share, work = work.id.sequence(),
            incarnation = ?work.id.incarnation());
        self.commit = Some(self.generation.spawn_blocking(move || {
            let result = store.execute(share, &work.operation);
            tracing::debug!(event = "storage_finished", ?share, work = work.id.sequence(),
                incarnation = ?work.id.incarnation(), complete = result.is_ok());
            (work.id, result)
        }));
        Ok(())
    }

    fn encode(&self, control: Control) -> Result<Vec<u8>> {
        Ok(match control {
            Control::Challenge(bytes) => bytes,
            Control::Error(error) => error.encode().to_vec(),
            Control::Registered(frame) => frame.encode().to_vec(),
            Control::Stopped(frame) => frame.encode().to_vec(),
            Control::ProbeAck(frame) => frame.encode().to_vec(),
            Control::Grant(grant) => grant.encode()?.to_vec(),
            Control::Settlement(notice) => notice.encode().to_vec(),
            Control::Descriptor { session, record } => {
                let authority = self
                    .authority
                    .as_ref()
                    .ok_or_else(|| anyhow!("descriptor authority missing"))?;
                ensure!(
                    authority.record() == *record,
                    "descriptor authority changed"
                );
                let descriptor = authority
                    .descriptor()
                    .ok_or_else(|| anyhow!("committed descriptor missing"))?;
                tracing::debug!(event = "session_descriptor_issued", share = ?self.identity.share, ?session);
                encode_descriptor(session.wire_id(), descriptor)?
            }
        })
    }
}
