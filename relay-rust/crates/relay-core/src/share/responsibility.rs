//! Independent lifecycle reservations fence delivery records that may disappear.
use super::checkpoint::ConnectionRole;
use super::checkpoint_codec::{bounded_count, field, put_header, read_header};
use super::{
    ConnectionCheckpoint, ConnectionRef, Detached, Error, Identity, Limits, Observer, ShareRelay,
};
use crate::{
    clock::UnixMillis,
    flow::{ConnectionLedger, Counts},
    wire::Reader,
};
use alloc::vec::Vec;

const MANIFEST_FORMAT: u8 = 2;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum GenerationState {
    Connected,
    Detached(Detached),
    Unavailable(Detached),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct Reservation {
    pub connection: ConnectionRef,
    pub bound: Counts,
    pub state: GenerationState,
}

/// Independently retained admission inventory, risk reservations and session-ID fence.
///
/// This contains no live delivery counters. Missing delivery records cannot erase a
/// reservation. Hosts commit membership and retirement transitions independently of
/// socket discovery; ordinary forwarding updates only connection checkpoints.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CheckpointManifest {
    pub(super) identity: Identity,
    pub(super) last_session: u64,
    pub(super) reservations: Vec<Reservation>,
}

impl CheckpointManifest {
    pub fn generations(&self) -> impl Iterator<Item = ConnectionRef> + '_ {
        self.reservations.iter().map(|entry| entry.connection)
    }

    #[must_use]
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        out.push(MANIFEST_FORMAT);
        put_header(&mut out, self.identity, self.last_session);
        out.extend_from_slice(&(self.reservations.len() as u64).to_be_bytes());
        for reservation in &self.reservations {
            out.extend_from_slice(&reservation.connection.encode());
            out.extend_from_slice(&reservation.bound.frames.to_be_bytes());
            out.extend_from_slice(&reservation.bound.encoded_bytes.to_be_bytes());
            let (kind, detached) = match reservation.state {
                GenerationState::Connected => (0, None),
                GenerationState::Detached(state) => (1, Some(state)),
                GenerationState::Unavailable(state) => (2, Some(state)),
            };
            out.push(kind);
            if let Some(state) = detached {
                match state {
                    Detached::AwaitingRelease => out.push(0),
                    Detached::Released => out.push(1),
                    Detached::Quiescing { not_before } => {
                        out.push(2);
                        out.extend_from_slice(&not_before.0.to_be_bytes());
                    }
                    Detached::Retired => out.push(3),
                }
            }
        }
        out
    }

    /// # Errors
    /// Rejects malformed, duplicate, unordered or over-capacity reservations.
    pub fn decode(bytes: &[u8], limits: Limits) -> Result<Self, Error> {
        let mut body = Reader::new(bytes);
        if field::<1>(&mut body)? != [MANIFEST_FORMAT] {
            return Err(Error::InvalidCheckpoint);
        }
        let (identity, last_session) = read_header(&mut body)?;
        let count = bounded_count(&mut body, limits.generation_limit()?)?;
        let mut reservations = Vec::with_capacity(count);
        for _ in 0..count {
            let connection =
                ConnectionRef::decode(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
            let bound = Counts::new(
                u64::from_be_bytes(field(&mut body)?),
                u64::from_be_bytes(field(&mut body)?),
            );
            let kind = field::<1>(&mut body)?[0];
            let state = if kind == 0 {
                GenerationState::Connected
            } else {
                let detached = match field::<1>(&mut body)? {
                    [0] => Detached::AwaitingRelease,
                    [1] => Detached::Released,
                    [2] => Detached::Quiescing {
                        not_before: UnixMillis(u64::from_be_bytes(field(&mut body)?)),
                    },
                    [3] => Detached::Retired,
                    _ => return Err(Error::InvalidCheckpoint),
                };
                match kind {
                    1 => GenerationState::Detached(detached),
                    2 => GenerationState::Unavailable(detached),
                    _ => return Err(Error::InvalidCheckpoint),
                }
            };
            reservations.push(Reservation {
                connection,
                bound,
                state,
            });
        }
        body.finish().map_err(|_| Error::InvalidCheckpoint)?;
        let manifest = Self {
            identity,
            last_session,
            reservations,
        };
        manifest.validate(limits)?;
        Ok(manifest)
    }

    pub(super) fn validate(&self, limits: Limits) -> Result<(), Error> {
        if self.reservations.len() > limits.generation_limit()?
            || self
                .reservations
                .windows(2)
                .any(|pair| pair[0].connection >= pair[1].connection)
        {
            return Err(Error::InvalidCheckpoint);
        }
        let mut retired = 0;
        let mut residual = Counts::ZERO;
        for entry in &self.reservations {
            if entry.bound.frames > limits.delivery.total().frames
                || entry.bound.encoded_bytes > limits.delivery.total().encoded_bytes
            {
                return Err(Error::InvalidCheckpoint);
            }
            match entry.state {
                GenerationState::Detached(Detached::Retired)
                | GenerationState::Unavailable(Detached::Retired) => {
                    retired += 1;
                    residual.frames = residual
                        .frames
                        .checked_add(entry.bound.frames)
                        .ok_or(Error::InvalidCheckpoint)?;
                    residual.encoded_bytes = residual
                        .encoded_bytes
                        .checked_add(entry.bound.encoded_bytes)
                        .ok_or(Error::InvalidCheckpoint)?;
                }
                GenerationState::Detached(Detached::Released)
                | GenerationState::Unavailable(Detached::Released) => {
                    if entry.bound != Counts::ZERO {
                        return Err(Error::InvalidCheckpoint);
                    }
                }
                _ if entry.bound != limits.delivery.total() => {
                    return Err(Error::InvalidCheckpoint);
                }
                _ => {}
            }
        }
        if retired > limits.retirement.generations()
            || self.reservations.len() - retired > limits.connections
        {
            return Err(Error::InvalidCheckpoint);
        }
        if let super::RetirementPolicy::Bounded {
            residual: limit, ..
        } = limits.retirement
            && (residual.frames > limit.frames || residual.encoded_bytes > limit.encoded_bytes)
        {
            return Err(Error::InvalidCheckpoint);
        }
        Ok(())
    }

    pub(super) fn accepts(&self, entry: &ConnectionCheckpoint, limits: Limits) -> bool {
        let Some(reservation) = self
            .reservations
            .iter()
            .find(|r| r.connection == entry.connection)
        else {
            return false;
        };
        if entry.identity != self.identity
            || entry.ledger.flows().any(|flow| {
                u64::from_be_bytes(*flow.session().wire_id().as_bytes()) > self.last_session
            })
            || entry.sessions.iter().any(|session| {
                u64::from_be_bytes(*session.key().wire_id().as_bytes()) > self.last_session
            })
            || entry.validate(limits).is_err()
        {
            return false;
        }
        let state_matches = match (reservation.state, &entry.role) {
            (
                GenerationState::Connected,
                ConnectionRole::Established(_)
                | ConnectionRole::Closing
                | ConnectionRole::Handshake(_),
            ) => true,
            (GenerationState::Detached(expected), ConnectionRole::Detached(actual)) => {
                expected == *actual
            }
            _ => false,
        };
        let Ok(ledger) = ConnectionLedger::restore(
            self.identity.relay,
            entry.connection,
            limits.delivery,
            limits.retained_flows_per_connection,
            &entry.ledger,
            (),
        ) else {
            return false;
        };
        state_matches
            && (!matches!(entry.role, ConnectionRole::Established(_))
                || ledger.receive_capacity().is_some())
            && (!matches!(entry.role, ConnectionRole::Detached(Detached::Retired))
                || ledger.queues_discarded())
            && ledger.usage().total().frames <= reservation.bound.frames
            && ledger.usage().total().encoded_bytes <= reservation.bound.encoded_bytes
    }
}

impl<O: Observer> ShareRelay<O> {
    /// Capture admission/retirement facts without live delivery-counter persistence.
    ///
    /// # Errors
    /// Unfinished durable lifecycle work must complete before capturing recovery state.
    pub fn checkpoint_manifest(&self) -> Result<CheckpointManifest, Error> {
        self.checkpoint_ready()?;
        let mut reservations: Vec<_> = self
            .ledgers
            .iter()
            .map(|(&connection, ledger)| {
                let state = self.detached.get(&connection).copied();
                let bound = match state {
                    Some(Detached::Retired) => ledger.usage().submitted,
                    Some(Detached::Released) => Counts::ZERO,
                    _ => self.limits.delivery.total(),
                };
                Reservation {
                    connection,
                    bound,
                    state: state.map_or(GenerationState::Connected, GenerationState::Detached),
                }
            })
            .collect();
        reservations.extend(
            self.unavailable
                .iter()
                .map(|(&connection, &bound)| Reservation {
                    connection,
                    bound,
                    state: GenerationState::Unavailable(self.detached[&connection]),
                }),
        );
        reservations.sort_unstable_by_key(|entry| entry.connection);
        Ok(CheckpointManifest {
            identity: self.identity,
            last_session: self.last_session,
            reservations,
        })
    }
}
