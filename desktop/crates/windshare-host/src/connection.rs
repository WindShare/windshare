use crate::request::{Command, Request};
use crate::{
    Connection, Shared, Snapshot,
    abi::{Endpoint, Host},
};
use anyhow::{Context as _, Result, bail};
use serde::Deserialize;
use std::{
    collections::BTreeMap,
    io::ErrorKind,
    net::{SocketAddr, TcpStream},
    sync::{atomic::Ordering, mpsc::Receiver},
    time::{Duration, Instant},
};
use tungstenite::{Message, WebSocket, client::IntoClientRequest};

#[cfg(test)]
#[path = "connection_tests.rs"]
mod tests;

#[cfg(test)]
#[path = "socket_tests.rs"]
mod socket_tests;

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
        let mut socket = connect(&host.endpoint)?;
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

fn connect(endpoint: &Endpoint) -> Result<WebSocket<TcpStream>> {
    let address: SocketAddr = endpoint.address.parse()?;
    let stream = TcpStream::connect_timeout(&address, IO_TIMEOUT)?;
    stream.set_read_timeout(Some(IO_TIMEOUT))?;
    stream.set_write_timeout(Some(IO_TIMEOUT))?;
    let mut request = format!("ws://{}/control", endpoint.address).into_client_request()?;
    request.headers_mut().insert(
        "Authorization",
        format!("Bearer {}", endpoint.token).parse()?,
    );
    let (socket, _) = tungstenite::client(request, stream)?;
    Ok(socket)
}

pub fn run(shared: &Shared, commands: &Receiver<Command>) -> Result<()> {
    let host = Host::start().context("start local engine host")?;
    tracing::info!(event = "desktop.host_started", session_id = %host.endpoint.session_id,
        log_directory = %host.endpoint.log_directory);
    shared.update(|model| model.session_id = Some(host.endpoint.session_id.clone()));
    let mut sequence = 0_u64;
    let mut pending = BTreeMap::new();
    while !shared.closing.load(Ordering::Acquire) {
        let result = connection_loop(
            &host.endpoint,
            shared,
            commands,
            &mut pending,
            &mut sequence,
        );
        if shared.closing.load(Ordering::Acquire) {
            break;
        }
        let error = result
            .err()
            .map_or_else(|| "control connection closed".into(), |e| format!("{e:#}"));
        shared.update(|model| model.connection = Connection::Reconnecting(error.clone()));
        tracing::warn!(event = "desktop.control_reconnecting", session_id = %host.endpoint.session_id, %error);
        shared
            .wake
            .wait(&mut polling::Events::new(), Some(RETRY_INTERVAL))?;
    }
    shared.update(|model| model.connection = Connection::Closing);
    let session_id = host.endpoint.session_id.clone();
    let started = Instant::now();
    tracing::info!(event = "desktop.host_shutdown_started", %session_id);
    let result = host.close();
    tracing::info!(event = "desktop.host_joined", %session_id,
        elapsed_ms = started.elapsed().as_millis(), success = result.is_ok());
    result
}

fn connection_loop(
    endpoint: &Endpoint,
    shared: &Shared,
    commands: &Receiver<Command>,
    pending: &mut BTreeMap<u64, Request>,
    sequence: &mut u64,
) -> Result<()> {
    let mut socket = connect(endpoint)?;
    socket.get_ref().set_read_timeout(None)?;
    socket.get_ref().set_write_timeout(None)?;
    socket.get_ref().set_nonblocking(true)?;
    // SAFETY: this worker owns the socket and deletes the registration before
    // dropping it, including every error path through pump.
    unsafe {
        shared
            .wake
            .add(socket.get_ref(), polling::Event::readable(0))?;
    }
    let result = pump(endpoint, shared, commands, pending, sequence, &mut socket);
    let removed = shared.wake.delete(socket.get_ref());
    result.and_then(|()| removed.map_err(Into::into))
}

fn pump(
    endpoint: &Endpoint,
    shared: &Shared,
    commands: &Receiver<Command>,
    pending: &mut BTreeMap<u64, Request>,
    sequence: &mut u64,
    socket: &mut WebSocket<TcpStream>,
) -> Result<()> {
    for command in pending.values() {
        send(socket, command)?;
    }
    shared.update(|model| model.connection = Connection::Connected);
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
            tracing::info!(event = "desktop.host_request", session_id = %endpoint.session_id,
                request_id = %request.id, command = ?request.command);
            // Numeric ordering preserves command order when reconnecting after
            // the tenth request; a lost reply reuses the original operation.
            pending.insert(*sequence, request);
            send(socket, &pending[sequence])?;
        }
        let pending_before_read = pending.len();
        loop {
            match socket.read() {
                Ok(Message::Text(text)) => receive(shared, pending, text.as_str())?,
                Ok(Message::Close(_)) => bail!("host closed control connection"),
                Ok(_) => {}
                Err(error) if would_block(&error) => break,
                Err(error) => return Err(error.into()),
            }
        }
        let writable = match socket.flush() {
            Ok(()) => false,
            Err(error) if would_block(&error) => true,
            Err(error) => return Err(error.into()),
        };
        shared
            .wake
            .modify(socket.get_ref(), polling::Event::new(0, true, writable))?;
        if pending.len() < pending_before_read {
            continue;
        }
        if !shared.closing.load(Ordering::Acquire) {
            shared.wake.wait(&mut polling::Events::new(), None)?;
        }
    }
    // HostStop owns closing sockets; waiting for a close handshake here would
    // add a second shutdown authority and delay joining the engine.
    Ok(())
}

fn would_block(error: &tungstenite::Error) -> bool {
    matches!(error, tungstenite::Error::Io(error) if error.kind() == ErrorKind::WouldBlock)
}

fn send(socket: &mut WebSocket<TcpStream>, request: &Request) -> Result<()> {
    match socket.send(Message::Text(serde_json::to_string(request)?.into())) {
        // Tungstenite retains an accepted frame until the next writable event.
        Ok(()) => Ok(()),
        Err(error) if would_block(&error) => Ok(()),
        Err(error) => Err(error.into()),
    }
}

fn receive(shared: &Shared, pending: &mut BTreeMap<u64, Request>, text: &str) -> Result<()> {
    let message: Envelope = serde_json::from_str(text)?;
    if message.version != 1 {
        bail!("unsupported host control protocol");
    }
    let acknowledged = message
        .id
        .parse::<u64>()
        .ok()
        .and_then(|id| pending.remove(&id));
    if message.error.is_none() && message.snapshot.is_none() {
        return Ok(());
    }
    shared.update(|model| {
        if let Some(error) = message.error {
            if let Some(Request {
                command: Command::Create { operation_id, .. } | Command::Repeat { operation_id, .. },
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
    });
    Ok(())
}
