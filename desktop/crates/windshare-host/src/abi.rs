use anyhow::{Context as _, Result, bail};
use native::Api;
use serde::Deserialize;

mod native;

#[cfg(test)]
#[path = "abi_tests.rs"]
mod tests;

const ABI_VERSION: u32 = 1;

#[derive(Clone, Deserialize)]
pub struct Endpoint {
    pub session_id: String,
    pub address: String,
    pub token: String,
    pub log_directory: String,
}

#[derive(Deserialize)]
struct Response {
    abi_version: u32,
    #[serde(default)]
    handle: u64,
    endpoint: Option<Endpoint>,
    #[serde(default)]
    error: String,
}

fn response(bytes: &[u8]) -> Result<Response> {
    let result: Response = serde_json::from_slice(bytes)?;
    if result.abi_version != ABI_VERSION {
        bail!("unsupported host ABI version {}", result.abi_version);
    }
    if !result.error.is_empty() {
        bail!("{}", result.error);
    }
    Ok(result)
}

pub struct Host {
    handle: u64,
    api: &'static Api,
    pub endpoint: Endpoint,
}

impl Host {
    pub fn start() -> Result<Self> {
        let api = Api::get()?;
        let result = response(api.start()?.bytes())?;
        Ok(Self {
            handle: result.handle,
            api,
            endpoint: result.endpoint.context("host endpoint missing")?,
        })
    }

    pub fn close(mut self) -> Result<()> {
        let handle = std::mem::take(&mut self.handle);
        response(self.api.stop(handle)?.bytes())?;
        Ok(())
    }
}

impl Drop for Host {
    fn drop(&mut self) {
        if self.handle != 0
            && let Err(error) = self
                .api
                .stop(self.handle)
                .and_then(|buffer| response(buffer.bytes()))
        {
            tracing::error!(event = "desktop.host_drop_failed", %error);
        }
    }
}
