use anyhow::{Context, Result, ensure};
use hyper::{HeaderMap, header};
use std::collections::BTreeSet;
use url::{Host, Url};

/// Browser origins are explicit deployment policy, independent of the relay's
/// public endpoint and of untrusted Host/Forwarded request headers.
#[derive(Clone, Debug, Default)]
pub struct OriginPolicy {
    allowed: BTreeSet<String>,
    allow_localhost: bool,
}

impl OriginPolicy {
    /// Missing Origin is allowed for native clients. The default policy rejects
    /// all browser origins; local development access is an explicit opt-in.
    ///
    /// # Errors
    /// Rejects invalid entries before a listener can report readiness.
    pub fn new(
        origins: impl IntoIterator<Item = impl AsRef<str>>,
        allow_localhost: bool,
    ) -> Result<Self> {
        let allowed = origins
            .into_iter()
            .map(|raw| {
                let raw = raw.as_ref();
                Origin::parse(raw)
                    .map(|origin| origin.serialized)
                    .with_context(|| format!("invalid allowed origin {raw:?}"))
            })
            .collect::<Result<_>>()?;
        Ok(Self {
            allowed,
            allow_localhost,
        })
    }

    pub(super) fn allows(&self, headers: &HeaderMap) -> bool {
        let mut values = headers.get_all(header::ORIGIN).iter();
        let Some(value) = values.next() else {
            return true;
        };
        if values.next().is_some() {
            return false;
        }
        value
            .to_str()
            .ok()
            .and_then(|raw| Origin::parse(raw).ok())
            .is_some_and(|origin| {
                self.allowed.contains(&origin.serialized)
                    || (self.allow_localhost && origin.localhost)
            })
    }
}

struct Origin {
    serialized: String,
    localhost: bool,
}

impl Origin {
    fn parse(raw: &str) -> Result<Self> {
        let (_, authority) = raw
            .split_once("://")
            .context("expected an HTTP(S) origin")?;
        // URL parsers deliberately repair browser navigation input. A policy
        // entry must not silently turn credentials, paths or escapes into access.
        ensure!(
            raw.is_ascii()
                && !raw
                    .bytes()
                    .any(|byte| byte.is_ascii_whitespace() || byte.is_ascii_control())
                && !authority.is_empty()
                && !authority.contains(['/', '\\', '?', '#', '@', '%', '*', ','])
                && !authority.ends_with(':'),
            "expected an origin without credentials, path, query or fragment"
        );
        let url = Url::parse(raw)?;
        ensure!(
            matches!(url.scheme(), "http" | "https") && url.port() != Some(0),
            "expected HTTP(S) and a nonzero port"
        );
        let host = url.host().context("missing origin host")?;
        let localhost = match host {
            Host::Domain(name) => name == "localhost",
            Host::Ipv4(address) => {
                let raw_host = authority.split(':').next().unwrap_or_default();
                ensure!(raw_host == address.to_string(), "noncanonical IPv4 origin");
                address == std::net::Ipv4Addr::LOCALHOST
            }
            Host::Ipv6(address) => address == std::net::Ipv6Addr::LOCALHOST,
        };
        Ok(Self {
            serialized: url.origin().ascii_serialization(),
            localhost,
        })
    }
}

#[cfg(test)]
#[path = "origin_tests.rs"]
mod tests;
