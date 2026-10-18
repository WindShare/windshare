use anyhow::{Context, Result, anyhow, bail};
use std::{
    future::Future,
    net::{IpAddr, SocketAddr},
    path::PathBuf,
};
use tokio::net::TcpListener;
use tokio_util::sync::CancellationToken;
use windshare_relay_native::{
    ConnectionLimits, Endpoint, OriginPolicy, Server, SourcePolicy, stun,
};

const DEFAULT_LISTEN: &str = "127.0.0.1:8787";

struct Options {
    listen: SocketAddr,
    database: PathBuf,
    public_endpoint: Option<String>,
    origins: OriginPolicy,
    sources: SourcePolicy,
    connections: ConnectionLimits,
    stun: stun::Config,
}

fn options(mut args: impl Iterator<Item = String>) -> Result<Options> {
    let mut listen = DEFAULT_LISTEN.parse()?;
    let mut database = None;
    let mut public_endpoint = None;
    let mut origins = Vec::new();
    let mut allow_localhost = false;
    let mut proxies: Option<Vec<IpAddr>> = None;
    let mut source_header = None;
    let mut connections = ConnectionLimits::default();
    let mut stun = stun::Config::default();
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
            "--trusted-proxies" => {
                proxies = Some(
                    value
                        .split(',')
                        .map(|peer| peer.trim().parse())
                        .collect::<std::result::Result<_, _>>()?,
                );
            }
            "--source-ip-header" => source_header = Some(value),
            "--max-business-connections" => connections.business = value.parse()?,
            "--max-control-connections" => connections.controls = value.parse()?,
            "--max-business-connections-per-source" => {
                connections.business_per_source = value.parse()?;
            }
            "--max-control-connections-per-source" => {
                connections.controls_per_source = value.parse()?;
            }
            "--stun-udp" => {
                stun.udp_addresses = if value.trim().is_empty() {
                    Vec::new()
                } else {
                    value
                        .split(',')
                        .map(|address| address.trim().parse())
                        .collect::<std::result::Result<_, _>>()?
                };
            }
            "--stun-admin" => {
                stun.admin_address = if value.trim().is_empty() {
                    None
                } else {
                    Some(value.parse()?)
                };
            }
            "--stun-requests-per-second" => stun.limits.requests_per_second = value.parse()?,
            "--stun-source-requests-per-second" => {
                stun.limits.source_requests_per_second = value.parse()?;
            }
            "--stun-max-sources" => stun.limits.max_sources = value.parse()?,
            _ => bail!("unknown option {arg}"),
        }
    }
    stun.validate()?;
    connections.validate()?;
    let sources = match (proxies, source_header) {
        (None, None) => SourcePolicy::default(),
        (Some(peers), Some(header)) => SourcePolicy::trusted_proxy(peers, &header)?,
        _ => bail!("--trusted-proxies and --source-ip-header must be configured together"),
    };
    Ok(Options {
        listen,
        database: database.context("required: --database <path>")?,
        public_endpoint,
        origins: OriginPolicy::new(origins, allow_localhost)?,
        sources,
        connections,
        stun,
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
    run(options, shutdown_signal()?).await
}

fn shutdown_signal() -> Result<impl Future<Output = ()>> {
    // Service managers stop Unix daemons with SIGTERM; it must cross the same
    // joined cleanup barrier as an interactive Ctrl-C.
    #[cfg(unix)]
    let mut terminate = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
    #[cfg(unix)]
    let mut interrupt = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::interrupt())?;
    #[cfg(windows)]
    let mut interrupt = tokio::signal::windows::ctrl_c()?;
    Ok(async move {
        #[cfg(unix)]
        let signal = tokio::select! {
            _ = interrupt.recv() => "interrupt",
            _ = terminate.recv() => "terminate",
        };
        #[cfg(windows)]
        let signal = {
            interrupt.recv().await;
            "interrupt"
        };
        tracing::info!(event = "shutdown_signal", signal);
    })
}

async fn run(options: Options, shutdown: impl Future<Output = ()>) -> Result<()> {
    let listener = TcpListener::bind(options.listen).await?;
    let address = listener.local_addr()?;
    let endpoint = Endpoint::parse(
        &options
            .public_endpoint
            .unwrap_or_else(|| format!("ws://{address}/v2/ws")),
    )?;
    tracing::info!(event = "http_policy", origins = ?options.origins);
    tracing::info!(event = "connection_policy", sources = ?options.sources, limits = ?options.connections);
    let stun = stun::Service::bind(options.stun)?;
    let server = Server::open(
        endpoint.clone(),
        options.origins,
        options.sources,
        options.connections,
        &options.database,
    )?;
    println!(
        "{}",
        serde_json::json!({
            "event": "listening", "address": address.to_string(), "endpoint": endpoint.as_str(),
            "capabilities": ["registration", "resume", "join", "delivery", "stop", "heartbeat", "healthz", "stun"],
            "stun_udp": stun.udp_addresses(), "stun_admin": stun.admin_address(),
        })
    );
    let stopping = CancellationToken::new();
    let relay = async {
        let result = server
            .serve(listener, async {
                shutdown.await;
                stopping.cancel();
            })
            .await;
        // Listener failure must also retire STUN, even without an OS signal.
        stopping.cancel();
        result
    };
    let (result, ()) = tokio::join!(relay, stun.serve(stopping.cancelled()));
    result
}

#[cfg(test)]
#[path = "main_tests.rs"]
mod tests;
