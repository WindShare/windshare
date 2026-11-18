use anyhow::{Result, bail};
use std::{future::Future, path::PathBuf};
use tokio::net::TcpListener;
use tokio_util::sync::CancellationToken;
use windshare_relay_native::{
    Endpoint, Server,
    node::{Configuration, Identity},
    stun,
};

const USAGE: &str = "WindShare native relay\nUsage: wsrelay --config <node.json>";

#[tokio::main]
async fn main() -> Result<()> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.as_slice() == ["--help"] || args.as_slice() == ["-h"] {
        println!("{USAGE}");
        return Ok(());
    }
    tracing_subscriber::fmt()
        .json()
        .with_writer(std::io::stderr)
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();
    let [flag, path] = args.as_slice() else {
        bail!("{USAGE}")
    };
    if flag != "--config" {
        bail!("{USAGE}");
    }
    let path = PathBuf::from(path);
    let config = Configuration::read(&path)?;
    run(config, path, shutdown_signal()?).await
}

fn shutdown_signal() -> Result<impl Future<Output = ()>> {
    #[cfg(unix)]
    let mut terminate = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
    #[cfg(unix)]
    let mut interrupt = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::interrupt())?;
    #[cfg(windows)]
    let mut interrupt = tokio::signal::windows::ctrl_c()?;
    Ok(async move {
        #[cfg(unix)]
        let signal = tokio::select! { _ = interrupt.recv() => "interrupt", _ = terminate.recv() => "terminate" };
        #[cfg(windows)]
        let signal = {
            interrupt.recv().await;
            "interrupt"
        };
        tracing::info!(event = "shutdown_signal", signal);
    })
}

#[expect(
    clippy::significant_drop_tightening,
    reason = "The host lease moves into configure and serve and must outlive transport children."
)]
async fn run(
    config: Configuration,
    path: PathBuf,
    shutdown: impl Future<Output = ()>,
) -> Result<()> {
    let listener = TcpListener::bind(config.listen).await?;
    let address = listener.local_addr()?;
    let endpoint = config.endpoint.as_ref().map_or_else(
        || Endpoint::parse(&format!("ws://{address}/v2/ws")),
        |endpoint| Endpoint::parse(endpoint),
    )?;
    let server = Server::open(
        endpoint.clone(),
        config.origin_policy()?,
        config.source_policy()?,
        config.connections,
        &config.database,
    )?;
    server
        .node()
        .identify(Identity::open(&config.identity_file)?)?;
    let configured = server.configure(config.node, Some(path))?;
    let stun = stun::Service::bind_with_node(config.stun, configured.node())?;
    println!(
        "{}",
        serde_json::json!({ "event":"listening", "address":address.to_string(), "endpoint":endpoint.as_str(),
        "capabilities":["registration","resume","join","delivery","stop","heartbeat","healthz","stun","account_admission"],
        "stun_udp":stun.udp_addresses(), "stun_admin":stun.admin_address() })
    );
    let stopping = CancellationToken::new();
    let stopped = stopping.clone();
    let relay = async move {
        let result = configured
            .serve(listener, async {
                shutdown.await;
                stopping.cancel();
            })
            .await;
        stopping.cancel();
        result
    };
    let (result, ()) = tokio::join!(relay, stun.serve(stopped.cancelled()));
    result
}
