use crate::request::{Command, Request};
use crate::{Connection, Shared, Snapshot, abi::Host};
use anyhow::{Context as _, Result, bail};
use serde::Deserialize;
use std::{
    collections::BTreeMap,
    io::ErrorKind,
    net::{SocketAddr, TcpStream},
    sync::{atomic::Ordering, mpsc::Receiver},
    thread,
    time::Duration,
};
use tungstenite::{Message, WebSocket, client::IntoClientRequest};

#[cfg(test)]
#[path = "connection_tests.rs"]
mod tests;

const POLL_INTERVAL: Duration = Duration::from_millis(100);
const IO_TIMEOUT: Duration = Duration::from_secs(2);
const RETRY_INTERVAL: Duration = Duration::from_millis(500);
const MAX_PENDING: usize = 32;

#[derive(Deserialize)]
struct Envelope {
    version: u32,
    #[serde(default)]
    id: String,
    snapshot: Option<Snapshot>,
    error: Option<crate::Failure>,
}

pub fn check_host() -> Result<()> {
    let host = Host::start()?;
    let result = (|| {
        let mut socket = connect(&host)?;
        socket.get_ref().set_read_timeout(Some(IO_TIMEOUT))?;
        let initial = socket.read()?.into_text()?;
        let message: Envelope = serde_json::from_str(initial.as_str())?;
        if message
            .snapshot
            .as_ref()
            .is_none_or(|snapshot| snapshot.session_id != host.endpoint.session_id)
        {
            bail!("host session mismatch");
        }
        Ok(())
    })();
    let closed = host.close();
    result.and(closed)
}

fn connect(host: &Host) -> Result<WebSocket<TcpStream>> {
    let address: SocketAddr = host.endpoint.address.parse()?;
    let stream = TcpStream::connect_timeout(&address, IO_TIMEOUT)?;
    stream.set_read_timeout(Some(IO_TIMEOUT))?;
    stream.set_write_timeout(Some(IO_TIMEOUT))?;
    let mut request = format!("ws://{}/control", host.endpoint.address).into_client_request()?;
    request.headers_mut().insert(
        "Authorization",
        format!("Bearer {}", host.endpoint.token).parse()?,
    );
    let (socket, _) = tungstenite::client(request, stream)?;
    socket.get_ref().set_read_timeout(Some(POLL_INTERVAL))?;
    Ok(socket)
}

pub fn run(shared: &Shared, commands: &Receiver<Command>) -> Result<()> {
    let host = Host::start().context("start local engine host")?;
    tracing::info!(event = "desktop.host_started", session_id = %host.endpoint.session_id,
        log_directory = %host.endpoint.log_directory);
    shared.model.lock().expect("model lock").session_id = Some(host.endpoint.session_id.clone());
    let mut sequence = 0_u64;
    let mut pending = BTreeMap::new();
    while !shared.closing.load(Ordering::Acquire) {
        let result = connection_loop(&host, shared, commands, &mut pending, &mut sequence);
        if shared.closing.load(Ordering::Acquire) {
            break;
        }
        let error = result
            .err()
            .map_or_else(|| "control connection closed".into(), |e| format!("{e:#}"));
        shared.model.lock().expect("model lock").connection =
            Connection::Reconnecting(error.clone());
        tracing::warn!(event = "desktop.control_reconnecting", session_id = %host.endpoint.session_id, %error);
        thread::sleep(RETRY_INTERVAL);
    }
    shared.model.lock().expect("model lock").connection = Connection::Closing;
    let result = host.close();
    tracing::info!(event = "desktop.host_joined", success = result.is_ok());
    result
}

fn connection_loop(
    host: &Host,
    shared: &Shared,
    commands: &Receiver<Command>,
    pending: &mut BTreeMap<u64, Request>,
    sequence: &mut u64,
) -> Result<()> {
    let mut socket = connect(host)?;
    for command in pending.values() {
        socket.send(Message::Text(serde_json::to_string(command)?.into()))?;
    }
    shared.model.lock().expect("model lock").connection = Connection::Connected;
    while !shared.closing.load(Ordering::Acquire) {
        while pending.len() < MAX_PENDING {
            let Ok(command) = commands.try_recv() else {
                break;
            };
            *sequence += 1;
            let request = Request {
                id: sequence.to_string(),
                command,
            };
            tracing::info!(event = "desktop.host_request", session_id = %host.endpoint.session_id,
                request_id = %request.id, command = ?request.command);
            // Numeric ordering preserves command order when reconnecting after
            // the tenth request; a lost reply reuses the original operation.
            pending.insert(*sequence, request);
            socket.send(Message::Text(
                serde_json::to_string(&pending[sequence])?.into(),
            ))?;
        }
        match socket.read() {
            Ok(Message::Text(text)) => receive(shared, pending, text.as_str())?,
            Ok(Message::Close(_)) => bail!("host closed control connection"),
            Ok(_) => {}
            Err(tungstenite::Error::Io(error))
                if matches!(error.kind(), ErrorKind::WouldBlock | ErrorKind::TimedOut) => {}
            Err(error) => return Err(error.into()),
        }
    }
    // HostStop owns closing sockets; waiting for a close handshake here would
    // add a second shutdown authority and delay joining the engine.
    Ok(())
}

fn receive(shared: &Shared, pending: &mut BTreeMap<u64, Request>, text: &str) -> Result<()> {
    let message: Envelope = serde_json::from_str(text)?;
    if message.version != 1 {
        bail!("unsupported host control protocol");
    }
    let mut model = shared.model.lock().expect("model lock");
    let acknowledged = message
        .id
        .parse::<u64>()
        .ok()
        .and_then(|id| pending.remove(&id));
    if let Some(error) = message.error {
        if let Some(Request {
            command: Command::Create { operation_id, .. },
            ..
        }) = acknowledged
        {
            model.reject(&operation_id, error);
        } else {
            model.command_error = Some(error.message);
        }
    }
    if let Some(snapshot) = message.snapshot {
        model.reconcile(snapshot);
    }
    drop(model);
    Ok(())
}
