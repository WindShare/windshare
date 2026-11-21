pub use super::endpoints::{public_origin, stun_host};
use crate::{Fault, Zone};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Deserializer, Serialize};
use std::collections::BTreeSet;

pub const MAX_NODES: usize = 256;
pub const MAX_COUNTER: u64 = (1 << 53) - 1;

/// # Errors
/// Rejects values other than a list or the historical null encoding.
pub fn null_list<'de, D: Deserializer<'de>, T: Deserialize<'de>>(d: D) -> Result<Vec<T>, D::Error> {
    Ok(Option::<Vec<T>>::deserialize(d)?.unwrap_or_default())
}

/// # Errors
/// Rejects noncanonical or empty Ed25519 public identities.
pub fn public_key(raw: &str) -> Result<[u8; 32]> {
    let key: [u8; 32] = URL_SAFE_NO_PAD
        .decode(raw)
        .ok()
        .and_then(|v| v.try_into().ok())
        .ok_or(Fault::Invalid)?;
    ensure!(key != [0; 32], Fault::Invalid);
    Ok(key)
}

pub fn valid_text(value: &str) -> bool {
    let edge = |c: char| c.is_whitespace() || c == '\u{feff}';
    !value.is_empty()
        && value.len() <= 128
        && value.trim_matches(edge) == value
        && !value.contains(['\0', '\r', '\n', '\t'])
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq, PartialOrd, Ord)]
#[serde(rename_all = "lowercase")]
pub enum Role {
    Relay,
    Stun,
}
#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum Status {
    Canary,
    Active,
    Draining,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct Spec {
    pub alias: String,
    #[serde(default)]
    pub node_key: String,
    pub relay: String,
    #[serde(default, deserialize_with = "null_list")]
    pub stun: Vec<String>,
    pub roles: Vec<Role>,
    pub zone: Zone,
    pub region: String,
    pub provider: String,
    pub operator: String,
    pub capacity_mbps: u32,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct Entry {
    #[serde(flatten)]
    pub spec: Spec,
    pub status: Status,
}
impl Spec {
    pub(super) fn validate(&self, history: bool) -> Result<()> {
        if !history || !self.node_key.is_empty() {
            public_key(&self.node_key)?;
        }
        self.relay_identity()?;
        let alias = self.alias.as_bytes();
        ensure!(
            alias.first().is_some_and(u8::is_ascii_lowercase)
                && alias.len() <= 32
                && !alias.ends_with(b"-")
                && alias
                    .iter()
                    .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || *b == b'-'),
            Fault::Invalid
        );
        ensure!(
            [&self.region, &self.provider, &self.operator]
                .into_iter()
                .all(|v| valid_text(v))
                && self.capacity_mbps > 0
                && !self.roles.is_empty()
                && self.roles.iter().collect::<BTreeSet<_>>().len() == self.roles.len()
                && self.roles.contains(&Role::Stun) != self.stun.is_empty()
                && self.stun.len() <= 4,
            Fault::Invalid
        );
        Ok(())
    }
    pub(super) fn relay_identity(&self) -> Result<String> {
        super::endpoints::relay_identity(&self.relay)
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct PublicStun {
    pub url: String,
    pub region: String,
    pub provider: String,
    pub failure_domain: String,
}
#[derive(Default)]
pub(super) struct Occupied {
    aliases: BTreeSet<String>,
    relays: BTreeSet<String>,
    stuns: BTreeSet<String>,
}
impl Occupied {
    pub(super) fn add_stun(&mut self, endpoint: &str) -> Result<()> {
        stun_host(endpoint)?;
        ensure!(
            self.stuns.insert(endpoint.into()),
            Fault::Conflict {
                reason: "stun_bound".into()
            }
        );
        Ok(())
    }
    pub(super) fn add(&mut self, spec: &Spec) -> Result<()> {
        ensure!(
            self.aliases.insert(spec.alias.clone()),
            Fault::Conflict {
                reason: "alias_bound".into()
            }
        );
        ensure!(
            self.relays.insert(spec.relay_identity()?),
            Fault::Conflict {
                reason: "relay_bound".into()
            }
        );
        for stun in &spec.stun {
            self.add_stun(stun)?;
        }
        Ok(())
    }
}
