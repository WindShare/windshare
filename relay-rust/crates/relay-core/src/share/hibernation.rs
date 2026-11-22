//! Opt-in hibernation snapshots for hosts that hibernate live sockets (such
//! as Cloudflare Durable Objects). Only established connections are captured;
//! unestablished handshakes and closing sockets reconnect cleanly on cold wake.
use super::{
    ConnectionPhase, ConnectionRef, Error, Identity, Limits, Observer, Output, Record,
    RelayDecision, RelayStep, RuntimeStatus, ShareLifecycle, ShareRelay,
    restoration::validate_limits,
};
use crate::{
    clock::UnixMillis,
    flow::{ConnectionLedger, Counts, LedgerCheckpoint, Phase as FlowPhase},
    heartbeat::Liveness,
    identifiers::{RelayIdentity, ShareId},
    lifecycle::{Incarnation, Phase as LifecyclePhase, Restoration, SessionRestore},
    observation::Journal,
    sessions::{Retirement, RetirementReason, SessionKey, SessionSnapshot},
    wire::Reader,
};
use alloc::{boxed::Box, vec::Vec};

const FORMAT: u8 = 1;
const LENGTH_BYTES: usize = size_of::<u64>();
const IDENTITY_BYTES: usize = 32 + 12 + 16;
const HEADER_BYTES: usize =
    1 + IDENTITY_BYTES + ConnectionRef::ENCODED_BYTES + Liveness::ENCODED_BYTES + 2 * LENGTH_BYTES;

/// Host attachment size allowance, independent of any platform's numeric limits.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AttachmentBudget {
    payload_bytes: usize,
}

impl AttachmentBudget {
    /// Reserve space for the host's structured serialization and own metadata.
    pub const fn new(attachment_bytes: usize, envelope_bytes: usize) -> Option<Self> {
        match attachment_bytes.checked_sub(envelope_bytes) {
            Some(payload_bytes) if payload_bytes > 0 => Some(Self { payload_bytes }),
            _ => None,
        }
    }

    pub const fn payload_bytes(self) -> usize {
        self.payload_bytes
    }

    /// Check the worst retained established state against the host allowance.
    ///
    /// # Errors
    /// Refuses bounds that could exceed this host's attachment allowance.
    pub fn check(self, retained_flow_limit: usize) -> Result<(), Error> {
        if ConnectionSnapshot::maximum_bytes(retained_flow_limit)
            .is_some_and(|maximum| maximum <= self.payload_bytes)
        {
            Ok(())
        } else {
            Err(Error::CheckpointCapacity)
        }
    }
}

/// Complete hibernation state for one established connection and its receiver-owned sessions.
#[derive(Clone, Debug)]
pub struct ConnectionSnapshot {
    identity: Identity,
    connection: ConnectionRef,
    liveness: Liveness,
    ledger: Box<LedgerCheckpoint>,
    sessions: Vec<SessionSnapshot>,
}

/// Cold-wake input for a hibernating host with surviving established sockets.
#[derive(Clone, Copy, Debug)]
pub struct Hibernated<'a> {
    pub record: Option<Record>,
    pub last_session: u64,
    pub surviving: &'a [ConnectionRef],
    pub connections: &'a [ConnectionSnapshot],
}

impl ConnectionSnapshot {
    pub const fn connection(&self) -> ConnectionRef {
        self.connection
    }

    /// Maximum binary size for one established connection under this flow limit.
    pub fn maximum_bytes(retained_flow_limit: usize) -> Option<usize> {
        let ledger = LedgerCheckpoint::maximum_bytes(retained_flow_limit)?;
        let sessions =
            retained_flow_limit.checked_mul(LENGTH_BYTES + crate::sessions::MAX_SNAPSHOT_BYTES)?;
        HEADER_BYTES.checked_add(ledger)?.checked_add(sessions)
    }

    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        out.push(FORMAT);
        out.extend_from_slice(self.identity.relay.as_bytes());
        out.extend_from_slice(self.identity.share.as_bytes());
        out.extend_from_slice(self.identity.incarnation.as_bytes());
        out.extend_from_slice(&self.connection.encode());
        out.extend_from_slice(&self.liveness.encode());
        put_block(&mut out, &self.ledger.encode());
        out.extend_from_slice(&(self.sessions.len() as u64).to_be_bytes());
        for session in &self.sessions {
            put_block(&mut out, session.encode().as_bytes());
        }
        out
    }

    /// # Errors
    /// Rejects oversized, malformed, or self-inconsistent connection records.
    pub fn decode(bytes: &[u8], limits: Limits) -> Result<Self, Error> {
        let maximum = Self::maximum_bytes(limits.retained_flows_per_connection)
            .ok_or(Error::InvalidCheckpoint)?;
        if bytes.len() > maximum {
            return Err(Error::InvalidCheckpoint);
        }
        let mut body = Reader::new(bytes);
        if field::<1>(&mut body)? != [FORMAT] {
            return Err(Error::InvalidCheckpoint);
        }
        let relay = RelayIdentity::new(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
        let share = ShareId::new(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
        let incarnation = Incarnation::new(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
        let identity = Identity {
            relay,
            share,
            incarnation,
        };
        let connection =
            ConnectionRef::decode(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
        let liveness = Liveness::decode(&field::<{ Liveness::ENCODED_BYTES }>(&mut body)?)
            .map_err(|_| Error::InvalidCheckpoint)?;
        let ledger_max = LedgerCheckpoint::maximum_bytes(limits.retained_flows_per_connection)
            .ok_or(Error::InvalidCheckpoint)?;
        let ledger = LedgerCheckpoint::decode(
            read_block(&mut body, ledger_max)?,
            limits.retained_flows_per_connection,
        )
        .map_err(|_| Error::InvalidCheckpoint)?;
        let count = bounded_count(&mut body, limits.sessions)?;
        let mut sessions = Vec::with_capacity(count);
        for _ in 0..count {
            sessions.push(
                SessionSnapshot::decode(read_block(
                    &mut body,
                    crate::sessions::MAX_SNAPSHOT_BYTES,
                )?)
                .map_err(|_| Error::InvalidCheckpoint)?,
            );
        }
        body.finish().map_err(|_| Error::InvalidCheckpoint)?;
        let snapshot = Self {
            identity,
            connection,
            liveness,
            ledger: Box::new(ledger),
            sessions,
        };
        snapshot.validate(limits)?;
        Ok(snapshot)
    }

    fn validate(&self, limits: Limits) -> Result<(), Error> {
        if self.connection != self.ledger.destination()
            || self.sessions.len() > limits.sessions
            || self
                .sessions
                .iter()
                .any(|session| session.key().receiver() != self.connection)
        {
            return Err(Error::InvalidCheckpoint);
        }
        let ledger = ConnectionLedger::restore(
            self.identity.relay,
            self.connection,
            limits.delivery,
            limits.retained_flows_per_connection,
            &self.ledger,
            (),
        )
        .map_err(|_| Error::InvalidCheckpoint)?;
        if ledger.receive_capacity().is_none() {
            return Err(Error::InvalidCheckpoint);
        }
        Ok(())
    }
}

impl<O: Observer> ShareRelay<O> {
    /// Highest wire session ID issued by this share. Hibernating hosts persist
    /// this watermark when it advances so late frames for collected sessions are
    /// discarded rather than closing healthy senders.
    pub const fn last_session(&self) -> u64 {
        self.sessions.last_issued()
    }

    /// Iterate all established business connections eligible for hibernation snapshots.
    pub fn established_connections(&self) -> impl Iterator<Item = ConnectionRef> + '_ {
        self.generations.established_ids()
    }

    /// Capture the hibernation snapshot for an established connection.
    /// Returns `Ok(None)` for unestablished, closing, or detached connections.
    ///
    /// # Errors
    /// Refuses snapshots while storage work, a STOP barrier, or unsubmitted payloads are pending.
    pub fn snapshot_connection(
        &self,
        connection: ConnectionRef,
    ) -> Result<Option<ConnectionSnapshot>, Error> {
        if self.runtime == RuntimeStatus::ReloadRequired
            || !self.tasks.is_empty()
            || self.stop_barrier.is_some()
        {
            return Err(Error::CheckpointBusy);
        }
        let Some((liveness, ledger)) = self.generations.established_entry(connection) else {
            return Ok(None);
        };
        if ledger.usage().received != Counts::ZERO {
            return Err(Error::CheckpointBusy);
        }
        let saved = ledger.checkpoint().map_err(|_| Error::CheckpointBusy)?;
        let sessions = ledger
            .flows()
            .filter_map(|(flow, _)| {
                let key = flow.session();
                (key.receiver() == connection)
                    .then(|| {
                        self.sessions
                            .entries()
                            .get(&key.wire_id())
                            .and_then(|entry| entry.session.snapshot())
                    })
                    .flatten()
            })
            .collect();
        Ok(Some(ConnectionSnapshot {
            identity: self.identity,
            connection,
            liveness,
            ledger: Box::new(saved),
            sessions,
        }))
    }

    /// Restores a share across hibernation from durable authority and surviving
    /// established-connection snapshots. Surviving sockets without a valid
    /// snapshot (such as interrupted handshakes or damaged attachments) receive
    /// `Output::Close` without disturbing healthy siblings.
    ///
    /// # Errors
    /// Rejects invalid limits, foreign/reused-incarnation snapshots, or an
    /// invalid durable record.
    pub fn restore_hibernated(
        identity: Identity,
        limits: Limits,
        hibernated: &Hibernated<'_>,
        observer: O,
        now: UnixMillis,
    ) -> Result<(Self, RelayStep), Error> {
        validate_limits(limits)?;
        validate_hibernated(identity, limits, hibernated)?;
        let live: Vec<_> = hibernated
            .connections
            .iter()
            .map(|entry| entry.connection)
            .collect();
        let snapshots: Vec<_> = hibernated
            .connections
            .iter()
            .flat_map(|entry| entry.sessions.iter().copied())
            .collect();
        let mut decisions = Vec::new();
        let (lifecycle, action) = ShareLifecycle::restore_hibernated(
            identity.relay,
            identity.share,
            Restoration {
                record: hibernated.record,
                connections: &live,
                sessions: &snapshots,
            },
            identity.incarnation,
            Journal::default(),
            now,
            |_, decision| decisions.push(decision),
        )
        .map_err(Error::Restore)?;
        let mut relay = Self::from_lifecycle(
            identity,
            limits,
            lifecycle,
            hibernated.record,
            hibernated.last_session,
            observer,
            now,
        )?;
        let mut step = RelayStep::default();
        if relay.lifecycle.phase() == LifecyclePhase::Published {
            relay.restore_established_connections(hibernated.connections, now)?;
        }
        for &connection in hibernated.surviving {
            if relay
                .generations
                .connection(&connection)
                .is_none_or(|entry| entry.phase() == ConnectionPhase::Closing)
            {
                step.outputs.push(Output::Close(connection));
            }
        }
        if relay.lifecycle.phase() == LifecyclePhase::Published {
            relay.restore_sessions(decisions, now, &mut step);
        }
        relay.action(action, None, now, &mut step);
        relay.progress_retirements(now);
        relay.expire(now, &mut step);
        relay.progress_stop(now, &mut step);
        relay.start_work(now, &mut step);
        step.deadline = relay.deadline();
        relay.trace(
            None,
            RelayDecision::Restored {
                connections: relay.generations.connected_count(),
                retained_generations: relay.generations.retained_count(),
            },
            now,
        );
        Ok((relay, step))
    }

    fn restore_established_connections(
        &mut self,
        connections: &[ConnectionSnapshot],
        now: UnixMillis,
    ) -> Result<(), Error> {
        for snapshot in connections {
            let ledger = ConnectionLedger::restore(
                self.identity.relay,
                snapshot.connection,
                self.limits.delivery,
                self.limits.retained_flows_per_connection,
                &snapshot.ledger,
                Journal::default(),
            )
            .map_err(|_| Error::InvalidCheckpoint)?;
            for (flow, _) in ledger.flows() {
                self.sessions
                    .advance_last_issued(u64::from_be_bytes(*flow.session().wire_id().as_bytes()));
            }
            for session in &snapshot.sessions {
                self.sessions
                    .advance_last_issued(u64::from_be_bytes(*session.key().wire_id().as_bytes()));
            }
            let usage = ledger.usage();
            let flows: Vec<_> = ledger.flows().map(|(flow, _)| flow).collect();
            self.generations
                .restore_established(snapshot.connection, snapshot.liveness, ledger);
            self.sessions.restore_routes(snapshot.connection, flows)?;
            self.trace(
                Some(snapshot.connection),
                RelayDecision::ResponsibilityRestored {
                    communicating: true,
                    usage,
                },
                now,
            );
        }
        Ok(())
    }

    fn restore_sessions(
        &mut self,
        decisions: Vec<SessionRestore>,
        now: UnixMillis,
        step: &mut RelayStep,
    ) {
        for decision in decisions {
            match decision {
                SessionRestore::Restored(session) => {
                    if let Some(flows) = self.session_flows(session.key()) {
                        self.sessions.install(session, &flows);
                    } else {
                        self.close(session.key().receiver(), step);
                        self.trace(
                            Some(session.key().receiver()),
                            RelayDecision::InvalidSnapshot,
                            now,
                        );
                    }
                }
                SessionRestore::Retired(retirement) => {
                    self.close(retirement.session.receiver(), step);
                }
            }
        }
        let destinations: Vec<_> = self.generations.ledger_ids().collect();
        for destination in destinations {
            let flows: Vec<_> = self
                .generations
                .ledger(&destination)
                .unwrap()
                .flows()
                .collect();
            for (flow, phase) in flows {
                let valid = self
                    .sessions
                    .entries()
                    .get(&flow.session().wire_id())
                    .is_some_and(|entry| {
                        entry
                            .session
                            .check_forward(&self.lifecycle, flow.permit(), now)
                            .is_ok()
                    });
                match phase {
                    FlowPhase::Open if !valid => {
                        let _ = self.generations.update_ledger(
                            destination,
                            |ledger| {
                                ledger.retire(
                                    Retirement {
                                        session: flow.session(),
                                        reason: RetirementReason::OwnerUnavailable,
                                    },
                                    now,
                                )
                            },
                            &mut self.observer,
                        );
                        step.outputs.push(Output::Discard(flow));
                    }
                    FlowPhase::Discarding => step.outputs.push(Output::Discard(flow)),
                    FlowPhase::Open | FlowPhase::Settling => {}
                }
            }
            self.apply_delivery_close(destination, now, step);
            self.collect_ledger(destination, now);
            if self
                .generations
                .connection(&destination)
                .is_some_and(|entry| entry.phase() == ConnectionPhase::Closing)
                && !step
                    .outputs
                    .iter()
                    .any(|output| matches!(output, Output::Close(value) if *value == destination))
            {
                step.outputs.push(Output::Close(destination));
            }
        }
    }

    fn session_flows(&self, key: SessionKey) -> Option<[crate::flow::FlowKey; 2]> {
        let find = |destination| {
            self.generations
                .ledger(&destination)?
                .flows()
                .find_map(|(flow, phase)| {
                    (flow.session() == key && phase == FlowPhase::Open).then_some(flow)
                })
        };
        Some([find(key.sender())?, find(key.receiver())?])
    }
}

fn validate_hibernated(
    identity: Identity,
    limits: Limits,
    hibernated: &Hibernated<'_>,
) -> Result<(), Error> {
    if hibernated.surviving.len() > limits.business_connections + limits.handshakes.concurrent
        || hibernated.connections.len() > limits.business_connections
    {
        return Err(Error::InvalidCheckpoint);
    }
    for (index, connection) in hibernated.surviving.iter().enumerate() {
        if hibernated.surviving[..index].contains(connection) {
            return Err(Error::InvalidCheckpoint);
        }
    }
    let mut total_sessions = 0_usize;
    for (index, entry) in hibernated.connections.iter().enumerate() {
        if entry.identity.relay != identity.relay
            || entry.identity.share != identity.share
            || entry.identity.incarnation == identity.incarnation
            || !hibernated.surviving.contains(&entry.connection)
            || hibernated.connections[..index]
                .iter()
                .any(|prior| prior.connection == entry.connection)
        {
            return Err(Error::InvalidCheckpoint);
        }
        entry.validate(limits)?;
        total_sessions = total_sessions
            .checked_add(entry.sessions.len())
            .ok_or(Error::InvalidCheckpoint)?;
    }
    if total_sessions > limits.sessions {
        return Err(Error::InvalidCheckpoint);
    }
    Ok(())
}

fn put_block(out: &mut Vec<u8>, bytes: &[u8]) {
    out.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
    out.extend_from_slice(bytes);
}

fn field<const N: usize>(body: &mut Reader<'_>) -> Result<[u8; N], Error> {
    body.array().map_err(|_| Error::InvalidCheckpoint)
}

fn bounded_count(body: &mut Reader<'_>, maximum: usize) -> Result<usize, Error> {
    let value =
        usize::try_from(u64::from_be_bytes(field(body)?)).map_err(|_| Error::InvalidCheckpoint)?;
    if value > maximum || value > body.remainder().len() {
        return Err(Error::InvalidCheckpoint);
    }
    Ok(value)
}

fn read_block<'a>(body: &mut Reader<'a>, maximum: usize) -> Result<&'a [u8], Error> {
    let length = bounded_count(body, maximum)?;
    body.take(length).map_err(|_| Error::InvalidCheckpoint)
}
