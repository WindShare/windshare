#[cfg(test)]
#[path = "configuration_tests.rs"]
mod tests;

use super::Settings;
use crate::{ConnectionLimits, Endpoint, OriginPolicy, SourcePolicy, stun};
use anyhow::{Context, Result, ensure};
use serde::Deserialize;
use std::{
    net::{IpAddr, SocketAddr},
    path::{Path, PathBuf},
};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Configuration {
    pub listen: SocketAddr,
    pub database: PathBuf,
    pub identity_file: PathBuf,
    pub endpoint: Option<String>,
    #[serde(default)]
    origins: Origins,
    proxy: Option<Proxy>,
    #[serde(default)]
    pub connections: ConnectionLimits,
    #[serde(default)]
    pub stun: stun::Config,
    pub node: Settings,
}

#[derive(Default, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
enum Origins {
    Public,
    #[default]
    Native,
    Allowlist {
        origins: Vec<String>,
        #[serde(default)]
        allow_localhost: bool,
    },
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Proxy {
    peers: Vec<IpAddr>,
    header: String,
}

impl Configuration {
    /// # Errors
    /// Rejects ambiguous endpoints, origins and authority configuration before binding.
    pub fn read(path: &Path) -> Result<Self> {
        Self::read_bytes(path, &std::fs::read(path)?)
    }

    pub(super) fn read_bytes(path: &Path, bytes: &[u8]) -> Result<Self> {
        let mut json: serde_json::Value = serde_json::from_slice(bytes)?;
        super::settings::zone_defaults(json.get_mut("node").context("missing node settings")?);
        let mut config: Self = serde_json::from_value(json)?;
        let parent = path.parent().unwrap_or_else(|| Path::new("."));
        if config.database.is_relative() {
            config.database = parent.join(&config.database);
        }
        if config.identity_file.is_relative() {
            config.identity_file = parent.join(&config.identity_file);
        }
        config.node.validate()?;
        config.connections.validate()?;
        config.stun.validate()?;
        config.origin_policy()?;
        config.source_policy()?;
        ensure!(
            config.endpoint.is_some() || !config.listen.ip().is_unspecified(),
            "wildcard listen requires an explicit public endpoint"
        );
        if let Some(endpoint) = &config.endpoint {
            Endpoint::parse(endpoint)?;
        }
        Ok(config)
    }

    /// # Errors
    /// Only exact HTTP(S) origins are accepted.
    pub fn origin_policy(&self) -> Result<OriginPolicy> {
        match &self.origins {
            Origins::Public => Ok(OriginPolicy::Public),
            Origins::Native => Ok(OriginPolicy::default()),
            Origins::Allowlist {
                origins,
                allow_localhost,
            } => OriginPolicy::new(origins.clone(), *allow_localhost),
        }
    }

    /// # Errors
    /// Proxy trust must name explicit peers and one source header.
    pub fn source_policy(&self) -> Result<SourcePolicy> {
        self.proxy.as_ref().map_or_else(
            || Ok(SourcePolicy::default()),
            |proxy| SourcePolicy::trusted_proxy(proxy.peers.clone(), &proxy.header),
        )
    }
}
