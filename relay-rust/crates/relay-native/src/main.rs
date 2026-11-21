use anyhow::{Result, bail};
use std::{
    fmt,
    future::Future,
    path::PathBuf,
    sync::{Arc, OnceLock},
};
use tokio::net::TcpListener;
use tokio_util::sync::CancellationToken;
use tracing::{Event, Subscriber};
use tracing_subscriber::{
    fmt::{
        FmtContext, FormatEvent, FormatFields,
        format::{Format, Json, Writer},
    },
    registry::LookupSpan,
};
use windshare_relay_native::{
    Endpoint, Server,
    node::{Configuration, Identity},
    stun,
};

const USAGE: &str = "WindShare native relay\nUsage: wsrelay --config <node.json>";

struct NodeJsonFormat {
    inner: Format<Json>,
    node_id_json: Arc<OnceLock<String>>,
}

impl NodeJsonFormat {
    fn new(node_id_json: Arc<OnceLock<String>>) -> Self {
        Self {
            inner: Format::default().json(),
            node_id_json,
        }
    }
}

impl<S, N> FormatEvent<S, N> for NodeJsonFormat
where
    S: Subscriber + for<'a> LookupSpan<'a>,
    N: for<'a> FormatFields<'a> + 'static,
{
    fn format_event(
        &self,
        ctx: &FmtContext<'_, S, N>,
        mut writer: Writer<'_>,
        event: &Event<'_>,
    ) -> fmt::Result {
        let Some(node_id_json) = self.node_id_json.get() else {
            return self.inner.format_event(ctx, writer, event);
        };
        let mut line = String::new();
        self.inner
            .format_event(ctx, Writer::new(&mut line), event)?;
        if let Some(rest) = line.strip_prefix('{') {
            writer.write_str("{\"node_id\":")?;
            writer.write_str(node_id_json)?;
            if !rest.starts_with('}') {
                writer.write_str(",")?;
            }
            writer.write_str(rest)
        } else {
            writer.write_str(&line)
        }
    }
}

#[tokio::main]
async fn main() -> Result<()> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.as_slice() == ["--help"] || args.as_slice() == ["-h"] {
        println!("{USAGE}");
        return Ok(());
    }
    let node_id_json = Arc::new(OnceLock::new());
    tracing_subscriber::fmt()
        .json()
        .event_format(NodeJsonFormat::new(Arc::clone(&node_id_json)))
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
    run(config, path, shutdown_signal()?, &node_id_json).await
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
    node_id_json: &OnceLock<String>,
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
    let identity = Identity::open(&config.identity_file)?;
    let _ = node_id_json.set(serde_json::to_string(&identity.id())?);
    server.node().identify(identity)?;
    let configured = server.configure(config.node, Some(path))?;
    let stun = stun::Service::bind_with_node(config.stun, configured.node())?;
    println!(
        "{}",
        serde_json::json!({
            "event": "listening", "address": address.to_string(), "endpoint": endpoint.as_str(),
            "relay_url": endpoint.relay_url(),
            "capabilities": ["registration", "resume", "join", "delivery", "stop", "heartbeat", "healthz", "stun", "account_admission"],
            "stun_udp": stun.udp_addresses(), "stun_admin": stun.admin_address(),
        })
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

#[cfg(test)]
#[path = "main_tests.rs"]
mod tests;
