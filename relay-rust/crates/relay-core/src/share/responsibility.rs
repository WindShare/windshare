//! Independent admission reservations fence records that may disappear.
use super::checkpoint::{ConnectionResources, ConnectionRole};
use super::checkpoint_codec::{bounded_count, field, put_header, read_header};
use super::{
    AttemptLimiter, ConnectionCheckpoint, ConnectionRef, Detached, Error, Identity, Limits,
    Observer, ShareRelay, admission::AttemptState,
};
use crate::{
    clock::UnixMillis,
    flow::{ConnectionLedger, Counts},
    wire::Reader,
};
use alloc::vec::Vec;

const MANIFEST_FORMAT: u8 = 3;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum GenerationState {
    Connected,
    Detached(Detached),
    Unavailable(Detached),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum ReservationKind {
    Control {
        deadline: UnixMillis,
    },
    Business {
        bound: Counts,
        state: GenerationState,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct Reservation {
    pub connection: ConnectionRef,
    pub kind: ReservationKind,
}

/// Admission inventory, conservative delivery reservations and the session-ID fence.
/// Ordinary delivery counters remain in connection checkpoints.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CheckpointManifest {
    pub(super) identity: Identity,
    pub(super) last_session: u64,
    pub(super) attempts: AttemptState,
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
        out.extend_from_slice(&self.attempts.tokens.to_be_bytes());
        out.extend_from_slice(&self.attempts.updated.0.to_be_bytes());
        out.extend_from_slice(&(self.reservations.len() as u64).to_be_bytes());
        for reservation in &self.reservations {
            out.extend_from_slice(&reservation.connection.encode());
            match reservation.kind {
                ReservationKind::Control { deadline } => {
                    out.push(0);
                    out.extend_from_slice(&deadline.0.to_be_bytes());
                }
                ReservationKind::Business { bound, state } => {
                    out.push(1);
                    out.extend_from_slice(&bound.frames.to_be_bytes());
                    out.extend_from_slice(&bound.encoded_bytes.to_be_bytes());
                    let (kind, detached) = match state {
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
        let attempts = AttemptState {
            tokens: u32::from_be_bytes(field(&mut body)?),
            updated: UnixMillis(u64::from_be_bytes(field(&mut body)?)),
        };
        let count = bounded_count(&mut body, limits.generation_limit()?)?;
        let mut reservations = Vec::with_capacity(count);
        for _ in 0..count {
            let connection =
                ConnectionRef::decode(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
            let kind = match field::<1>(&mut body)? {
                [0] => ReservationKind::Control {
                    deadline: UnixMillis(u64::from_be_bytes(field(&mut body)?)),
                },
                [1] => {
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
                    ReservationKind::Business { bound, state }
                }
                _ => return Err(Error::InvalidCheckpoint),
            };
            reservations.push(Reservation { connection, kind });
        }
        body.finish().map_err(|_| Error::InvalidCheckpoint)?;
        let manifest = Self {
            identity,
            last_session,
            attempts,
            reservations,
        };
        manifest.validate(limits)?;
        Ok(manifest)
    }

    pub(super) fn validate(&self, limits: Limits) -> Result<(), Error> {
        AttemptLimiter::restore(limits.handshakes.attempts, self.attempts)
            .map_err(|_| Error::InvalidCheckpoint)?;
        if self.reservations.len() > limits.generation_limit()?
            || self
                .reservations
                .windows(2)
                .any(|pair| pair[0].connection >= pair[1].connection)
        {
            return Err(Error::InvalidCheckpoint);
        }
        let mut controls = 0;
        let mut retired = 0;
        let mut residual = Counts::ZERO;
        for entry in &self.reservations {
            let ReservationKind::Business { bound, state } = entry.kind else {
                controls += 1;
                continue;
            };
            if bound.frames > limits.delivery.total().frames
                || bound.encoded_bytes > limits.delivery.total().encoded_bytes
            {
                return Err(Error::InvalidCheckpoint);
            }
            match state {
                GenerationState::Detached(Detached::Retired)
                | GenerationState::Unavailable(Detached::Retired) => {
                    retired += 1;
                    residual.frames = residual
                        .frames
                        .checked_add(bound.frames)
                        .ok_or(Error::InvalidCheckpoint)?;
                    residual.encoded_bytes = residual
                        .encoded_bytes
                        .checked_add(bound.encoded_bytes)
                        .ok_or(Error::InvalidCheckpoint)?;
                }
                GenerationState::Detached(Detached::Released)
                | GenerationState::Unavailable(Detached::Released) => {
                    if bound != Counts::ZERO {
                        return Err(Error::InvalidCheckpoint);
                    }
                }
                _ if bound != limits.delivery.total() => return Err(Error::InvalidCheckpoint),
                _ => {}
            }
        }
        if controls > limits.handshakes.concurrent
            || retired > limits.retirement.generations()
            || self.reservations.len() - retired - controls > limits.business_connections
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
        if entry.identity != self.identity || entry.validate(limits).is_err() {
            return false;
        }
        let (bound, state, ledger, sessions) = match (reservation.kind, &entry.resources) {
            (
                ReservationKind::Control { deadline: expected },
                ConnectionResources::Control { deadline },
            ) => {
                return expected == *deadline;
            }
            (
                ReservationKind::Business { bound, state },
                ConnectionResources::Business { ledger, sessions },
            ) => (bound, state, ledger, sessions),
            _ => return false,
        };
        if ledger.flows().any(|flow| {
            u64::from_be_bytes(*flow.session().wire_id().as_bytes()) > self.last_session
        }) || sessions.iter().any(|session| {
            u64::from_be_bytes(*session.key().wire_id().as_bytes()) > self.last_session
        }) {
            return false;
        }
        let state_matches = match (state, &entry.role) {
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
            ledger,
            (),
        ) else {
            return false;
        };
        state_matches
            && (!matches!(entry.role, ConnectionRole::Established(_))
                || ledger.receive_capacity().is_some())
            && (!matches!(entry.role, ConnectionRole::Detached(Detached::Retired))
                || ledger.queues_discarded())
            && ledger.usage().total().frames <= bound.frames
            && ledger.usage().total().encoded_bytes <= bound.encoded_bytes
    }
}

impl<O: Observer> ShareRelay<O> {
    /// Capture admission facts without live delivery-counter persistence.
    ///
    /// # Errors
    /// Unfinished lifecycle work must complete before capturing recovery state.
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
                    kind: ReservationKind::Business {
                        bound,
                        state: state.map_or(GenerationState::Connected, GenerationState::Detached),
                    },
                }
            })
            .collect();
        reservations.extend(
            self.unavailable
                .iter()
                .map(|(&connection, &bound)| Reservation {
                    connection,
                    kind: ReservationKind::Business {
                        bound,
                        state: GenerationState::Unavailable(self.detached[&connection]),
                    },
                }),
        );
        reservations.extend(
            self.controls
                .iter()
                .map(|(&connection, &deadline)| Reservation {
                    connection,
                    kind: ReservationKind::Control { deadline },
                }),
        );
        reservations.sort_unstable_by_key(|entry| entry.connection);
        Ok(CheckpointManifest {
            identity: self.identity,
            last_session: self.last_session,
            attempts: self.handshake_rate.state(),
            reservations,
        })
    }
}
