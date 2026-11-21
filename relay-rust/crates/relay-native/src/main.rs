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

const BUSY_EXIT_CODE: i32 = 75;
const USAGE: &str = "WindShare native relay\nUsage: wsrelay --config <node.json> | --check-config <node.json> [node_key] | --enter-maintenance <config> <node_key> <operation> <policy_version> <reconnect_budget_ms> | --release-maintenance <config> <node_key> <operation> | --force-release-maintenance <config> <node_key> | --check-hold <config> <node_key> <operation>";

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
    if let Some(code) = run_maintenance_command(&args)? {
        if code != 0 {
            std::process::exit(code);
        }
        return Ok(());
    }
    let [flag, path] = args.as_slice() else {
        bail!("{USAGE}")
    };
    if flag == "--check-config" {
        Configuration::read(path.as_ref())?;
        return Ok(());
    }
    if flag != "--config" {
        bail!("{USAGE}");
    }
    let path = PathBuf::from(path);
    let config = Configuration::read(&path)?;
    run(config, path, shutdown_signal()?, &node_id_json).await
}

fn run_maintenance_command(args: &[String]) -> Result<Option<i32>> {
    use windshare_relay_native::node::maintenance::{self, Transition};
    match args {
        [command, path, node, operation, version, millis] if command == "--enter-maintenance" => {
            let transition = Transition::Enter {
                policy_version: version.parse()?,
                reconnect_budget: std::time::Duration::from_millis(millis.parse()?),
            };
            let done = maintenance::change(
                &Configuration::read(path.as_ref())?,
                node,
                operation,
                transition,
            )?;
            Ok(Some(if done { 0 } else { BUSY_EXIT_CODE }))
        }
        [command, path, node, operation] if command == "--release-maintenance" => {
            let done = maintenance::change(
                &Configuration::read(path.as_ref())?,
                node,
                operation,
                Transition::Release,
            )?;
            Ok(Some(if done { 0 } else { BUSY_EXIT_CODE }))
        }
        [command, path, node] if command == "--force-release-maintenance" => {
            let done = maintenance::force_release(&Configuration::read(path.as_ref())?, node)?;
            Ok(Some(if done { 0 } else { BUSY_EXIT_CODE }))
        }
        [command, path, node, operation] if command == "--check-hold" => {
            maintenance::check_hold(&Configuration::read(path.as_ref())?, node, operation)?;
            Ok(Some(0))
        }
        [command, path, node] if command == "--check-config" => {
            maintenance::check_identity(&Configuration::read(path.as_ref())?, node)?;
            Ok(Some(0))
        }
        _ => Ok(None),
    }
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
