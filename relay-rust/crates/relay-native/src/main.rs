use anyhow::{Context, Result, anyhow, bail};
use std::{net::SocketAddr, path::PathBuf};
use tokio::net::TcpListener;
use windshare_relay_native::{Endpoint, Server};

struct Options {
    listen: SocketAddr,
    database: PathBuf,
    public_endpoint: Option<String>,
}

fn options() -> Result<Options> {
    let mut listen = "127.0.0.1:8787".parse()?;
    let mut database = None;
    let mut public_endpoint = None;
    let mut args = std::env::args().skip(1);
    while let Some(arg) = args.next() {
        let value = args
            .next()
            .ok_or_else(|| anyhow!("missing value for {arg}"))?;
        match arg.as_str() {
            "--listen" => listen = value.parse()?,
            "--database" => database = Some(PathBuf::from(value)),
            "--public-endpoint" => public_endpoint = Some(value),
            _ => bail!("unknown option {arg}"),
        }
    }
    Ok(Options {
        listen,
        database: database.context("required: --database <path>")?,
        public_endpoint,
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
    let options = options()?;
    let listener = TcpListener::bind(options.listen).await?;
    let address = listener.local_addr()?;
    let endpoint = Endpoint::parse(
        &options
            .public_endpoint
            .unwrap_or_else(|| format!("ws://{address}/v2/ws")),
    )?;
    let server = Server::open(endpoint.clone(), &options.database)?;
    println!(
        "{}",
        serde_json::json!({
            "event": "listening", "address": address.to_string(), "endpoint": endpoint.as_str(),
            "capabilities": ["registration", "resume", "join", "delivery", "stop", "heartbeat"],
        })
    );
    server
        .serve(listener, async {
            let _ = tokio::signal::ctrl_c().await;
        })
        .await
}
