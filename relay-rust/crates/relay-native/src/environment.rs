use anyhow::{Result, anyhow};
use std::time::{Instant, SystemTime, UNIX_EPOCH};
use windshare_relay_core::{
    challenge::ChallengeSeed,
    clock::UnixMillis,
    lifecycle::Incarnation,
    observation::{Observation, Observer},
};

pub trait Environment: Clone + Send + Sync + 'static {
    fn now(&self) -> UnixMillis;
    fn challenge(&self) -> Result<ChallengeSeed>;
}

#[derive(Clone)]
pub struct NativeEnvironment {
    epoch: UnixMillis,
    started: Instant,
}

impl NativeEnvironment {
    pub fn new() -> Result<Self> {
        Ok(Self {
            epoch: UnixMillis(u64::try_from(
                SystemTime::now().duration_since(UNIX_EPOCH)?.as_millis(),
            )?),
            started: Instant::now(),
        })
    }
}

impl Environment for NativeEnvironment {
    fn now(&self) -> UnixMillis {
        // Absolute persisted deadlines still use Unix time; a wall-clock adjustment
        // during this process cannot renew a handshake or move its timer backwards.
        UnixMillis(
            self.epoch.0.saturating_add(
                u64::try_from(self.started.elapsed().as_millis()).unwrap_or(u64::MAX),
            ),
        )
    }

    fn challenge(&self) -> Result<ChallengeSeed> {
        ChallengeSeed::new(random()?, random()?).ok_or_else(|| anyhow!("zero challenge entropy"))
    }
}

fn random<const N: usize>() -> Result<[u8; N]> {
    let mut bytes = [0; N];
    getrandom::fill(&mut bytes).map_err(|error| anyhow!("OS entropy: {error}"))?;
    Ok(bytes)
}

pub fn incarnation() -> Result<Incarnation> {
    Incarnation::new(random()?).ok_or_else(|| anyhow!("zero incarnation entropy"))
}

#[derive(Debug)]
pub struct Trace;

impl Observer for Trace {
    fn observe(&mut self, observation: Observation) {
        match observation {
            Observation::Lifecycle(value) => tracing::info!(
                platform = "native", event = "lifecycle_decision", at = value.at.0,
                share = ?value.share, incarnation = ?value.incarnation,
                operation = ?value.operation, connection = ?value.connection,
                cause = ?value.event, before = ?value.before, after = ?value.after,
                decision = ?value.decision,
            ),
            Observation::Relay {
                at,
                share,
                incarnation,
                connection,
                decision,
                ..
            } => tracing::debug!(
                platform = "native",
                event = "relay_decision",
                at = at.0,
                ?share,
                ?incarnation,
                ?connection,
                ?decision
            ),
            value => {
                tracing::debug!(platform = "native", event = "delivery_decision", observation = ?value);
            }
        }
    }
}
