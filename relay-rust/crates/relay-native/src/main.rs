use anyhow::{Context, Result, anyhow, bail};
use std::{net::SocketAddr, path::PathBuf};
use tokio::net::TcpListener;
use windshare_relay_native::{Endpoint, OriginPolicy, Server};

const DEFAULT_LISTEN: &str = "127.0.0.1:8787";

struct Options {
    listen: SocketAddr,
    database: PathBuf,
    public_endpoint: Option<String>,
    origins: OriginPolicy,
}

fn options(mut args: impl Iterator<Item = String>) -> Result<Options> {
    let mut listen = DEFAULT_LISTEN.parse()?;
    let mut database = None;
    let mut public_endpoint = None;
    let mut origins = Vec::new();
    let mut allow_localhost = false;
    while let Some(arg) = args.next() {
        if arg == "--allow-localhost" {
            allow_localhost = true;
            continue;
        }
        let value = args
            .next()
            .ok_or_else(|| anyhow!("missing value for {arg}"))?;
        match arg.as_str() {
            "--listen" => listen = value.parse()?,
            "--database" => database = Some(PathBuf::from(value)),
            "--public-endpoint" => public_endpoint = Some(value),
            "--origins" => origins.extend(value.split(',').map(|origin| origin.trim().to_owned())),
            _ => bail!("unknown option {arg}"),
        }
    }
    Ok(Options {
        listen,
        database: database.context("required: --database <path>")?,
        public_endpoint,
        origins: OriginPolicy::new(origins, allow_localhost)?,
    })
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .json()
        .with_writer(std::io::stderr)
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();
    let options = options(std::env::args().skip(1))?;
    let listener = TcpListener::bind(options.listen).await?;
    let address = listener.local_addr()?;
    let endpoint = Endpoint::parse(
        &options
            .public_endpoint
            .unwrap_or_else(|| format!("ws://{address}/v2/ws")),
    )?;
    tracing::info!(event = "http_policy", origins = ?options.origins);
    let server = Server::open(endpoint.clone(), options.origins, &options.database)?;
    println!(
        "{}",
        serde_json::json!({
            "event": "listening", "address": address.to_string(), "endpoint": endpoint.as_str(),
            "capabilities": ["registration", "resume", "join", "delivery", "stop", "heartbeat", "healthz"],
        })
    );
    server
        .serve(listener, async {
            let _ = tokio::signal::ctrl_c().await;
        })
        .await
}

#[cfg(test)]
#[path = "main_tests.rs"]
mod tests;
