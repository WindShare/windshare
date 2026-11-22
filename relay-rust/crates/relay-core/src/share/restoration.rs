use super::{
    Error, Identity, Limits, Observer, Record, RelayDecision, RelayStep, RuntimeStatus,
    ShareLifecycle, ShareRelay,
};
use crate::{clock::UnixMillis, observation::Journal};
use alloc::collections::VecDeque;

impl<O: Observer> ShareRelay<O> {
    /// Restores a share from durable authority after a cold start or process
    /// restart (no surviving sockets).
    ///
    /// # Errors
    /// Rejects invalid limits or a stored record belonging to another relay,
    /// share, or incarnation.
    pub fn restore(
        identity: Identity,
        limits: Limits,
        record: Option<Record>,
        observer: O,
        now: UnixMillis,
    ) -> Result<(Self, RelayStep), Error> {
        validate_limits(limits)?;
        let (lifecycle, action) = ShareLifecycle::restore(
            identity.relay,
            identity.share,
            record,
            identity.incarnation,
            Journal::default(),
            now,
        )
        .map_err(Error::Restore)?;
        let mut relay =
            Self::from_lifecycle(identity, limits, lifecycle, record, 0, observer, now)?;
        let mut step = RelayStep::default();
        relay.action(action, None, now, &mut step);
        relay.expire(now, &mut step);
        relay.progress_stop(now, &mut step);
        relay.start_work(now, &mut step);
        step.deadline = relay.deadline();
        relay.trace(
            None,
            RelayDecision::Restored {
                connections: 0,
                retained_generations: 0,
            },
            now,
        );
        Ok((relay, step))
    }

    pub(super) fn from_lifecycle(
        identity: Identity,
        limits: Limits,
        lifecycle: ShareLifecycle<Journal>,
        stored: Option<Record>,
        last_session: u64,
        observer: O,
        now: UnixMillis,
    ) -> Result<Self, Error> {
        let mut relay = Self {
            identity,
            limits,
            lifecycle,
            generations: super::Generations::new(
                identity,
                limits,
                super::AttemptLimiter::new(limits.handshakes.attempts, now)?,
            ),
            sessions: super::sessions::Sessions::new(last_session),
            stored,
            runtime: RuntimeStatus::Running,
            last_work: 0,
            tasks: VecDeque::new(),
            stop_barrier: None,
            observer,
        };
        relay.flush_lifecycle();
        Ok(relay)
    }
}

pub(super) const fn validate_limits(limits: Limits) -> Result<(), Error> {
    if limits.business_connections == 0
        || limits.handshakes.concurrent == 0
        || limits.sessions == 0
        || limits.retained_flows_per_connection == 0
        || limits
            .business_connections
            .checked_add(limits.handshakes.concurrent)
            .is_none()
    {
        return Err(Error::Configuration);
    }
    Ok(())
}
