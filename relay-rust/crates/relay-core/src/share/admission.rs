//! Short control lifetimes never reserve payload capacity or business admission.
use super::{ConnectionRef, Error, Key, Observer, RelayDecision, ShareRelay};
use crate::{clock::UnixMillis, flow::ConnectionLedger, observation::Journal};
use core::{num::NonZeroU64, time::Duration};

pub const CONTROL_LIFETIME: Duration = Duration::from_secs(30);

#[derive(Clone, Copy, Debug)]
pub struct HandshakeLimits {
    /// First-frame and STOP sockets share this bounded control reserve.
    pub concurrent: usize,
    /// Applies to new connections; promotion and closure never refund attempts.
    pub attempts: AttemptRate,
}

/// One token is replenished per interval; rejected or closed attempts are not refunded.
#[derive(Clone, Copy, Debug)]
pub struct AttemptRate {
    pub burst: u32,
    pub refill: Duration,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct AttemptState {
    pub tokens: u32,
    pub updated: UnixMillis,
}

#[derive(Clone, Copy, Debug)]
pub struct AttemptLimiter {
    burst: u32,
    refill_millis: NonZeroU64,
    state: AttemptState,
}

impl AttemptLimiter {
    /// # Errors
    /// Refuses zero or unrepresentable rate bounds.
    pub fn new(rate: AttemptRate, now: UnixMillis) -> Result<Self, Error> {
        Self::restore(
            rate,
            AttemptState {
                tokens: rate.burst,
                updated: now,
            },
        )
    }

    pub(super) fn restore(rate: AttemptRate, state: AttemptState) -> Result<Self, Error> {
        let refill_millis = u64::try_from(rate.refill.as_millis())
            .ok()
            .and_then(NonZeroU64::new)
            .ok_or(Error::Configuration)?;
        if rate.burst == 0 || state.tokens > rate.burst {
            return Err(Error::Configuration);
        }
        Ok(Self {
            burst: rate.burst,
            refill_millis,
            state,
        })
    }

    pub(super) const fn state(self) -> AttemptState {
        self.state
    }

    /// A backwards clock neither replenishes tokens nor moves the refill origin.
    pub fn attempt(&mut self, now: UnixMillis) -> bool {
        let interval = self.refill_millis.get();
        let elapsed = now.0.saturating_sub(self.state.updated.0);
        let refill = elapsed / interval;
        if refill != 0 {
            self.state.tokens = self
                .state
                .tokens
                .saturating_add(u32::try_from(refill).unwrap_or(u32::MAX))
                .min(self.burst);
            self.state.updated.0 = now.0 - elapsed % interval;
        }
        if self.state.tokens == 0 {
            return false;
        }
        self.state.tokens -= 1;
        true
    }
}

impl<O: Observer> ShareRelay<O> {
    pub(super) fn admit_business(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
    ) -> Result<(), Error> {
        // Replay of an admitted handshake must preserve its original reservation.
        if self.ledgers.contains_key(&connection) {
            return Ok(());
        }
        if !self.controls.contains_key(&connection) {
            return Err(Error::UnknownConnection);
        }
        let usage = self.connection_usage();
        if usage.business >= self.limits.business_connections {
            self.trace(
                Some(connection),
                RelayDecision::BusinessCapacity { usage },
                now,
            );
            return Err(Error::Capacity);
        }
        let ledger = ConnectionLedger::new(
            self.identity.relay,
            connection,
            self.limits.delivery,
            self.limits.retained_flows_per_connection,
            Journal::default(),
        )
        .map_err(|_| Error::Configuration)?;
        self.ledgers.insert(connection, ledger);
        self.controls.remove(&connection);
        self.deadlines.set(Key::Control(connection), None);
        self.checkpointing.changed(connection);
        self.checkpointing.retention_changed();
        self.trace(Some(connection), RelayDecision::BusinessAdmitted, now);
        Ok(())
    }

    pub(super) fn release_control(&mut self, connection: ConnectionRef) {
        if self.controls.remove(&connection).is_some() {
            self.deadlines.set(Key::Control(connection), None);
            self.checkpointing.removed(connection);
        }
    }
}
