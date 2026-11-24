use crate::request::{Command, Request};
use crate::{
    Connection, SenderTraceChange, Shared, Snapshot,
    abi::{Endpoint, Host},
};
use anyhow::{Context as _, Result, bail};
use registered_socket::RegisteredSocket;
use serde::Deserialize;
use std::{
    collections::BTreeMap,
    io::ErrorKind,
    net::{SocketAddr, TcpStream},
    sync::{atomic::Ordering, mpsc::Receiver},
    time::{Duration, Instant},
};
use tungstenite::{Message, WebSocket, client::IntoClientRequest};

mod registered_socket;

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
    shared.update(|draft| {
        let session_id = Some(host.endpoint.session_id.clone());
        let log_directory = (!host.endpoint.log_directory.is_empty())
            .then(|| host.endpoint.log_directory.as_str().into());
        if draft.read().session_id != session_id || draft.read().log_directory != log_directory {
            let model = draft.write();
            model.session_id = session_id;
            model.log_directory = log_directory;
        }
    });
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
        shared.update(|draft| draft.set_connection(Connection::Reconnecting(error.clone())));
        tracing::warn!(event = "desktop.control_reconnecting", session_id = %host.endpoint.session_id, %error);
        shared
            .wake
            .wait(&mut polling::Events::new(), Some(RETRY_INTERVAL))?;
    }
    shared.update(|draft| draft.set_connection(Connection::Closing));
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
    let socket = connect(endpoint)?;
    socket.get_ref().set_read_timeout(None)?;
    socket.get_ref().set_write_timeout(None)?;
    socket.get_ref().set_nonblocking(true)?;
    let mut socket = RegisteredSocket::new(socket, &shared.wake, &endpoint.session_id)?;
    let result = pump(endpoint, shared, commands, pending, sequence, &mut socket);
    let removed = socket.close();
    result.and_then(|()| removed.map_err(Into::into))
}

fn pump(
    endpoint: &Endpoint,
    shared: &Shared,
    commands: &Receiver<Command>,
    pending: &mut BTreeMap<u64, Request>,
    sequence: &mut u64,
    socket: &mut RegisteredSocket<'_>,
) -> Result<()> {
    // Visibility is owned by each socket. Replay only application commands.
    pending.retain(|_, request| !matches!(request.command, Command::SetAccountView { .. }));
    for command in pending.values() {
        send(socket, command)?;
    }
    shared.update(|draft| draft.set_connection(Connection::Connected));
    let mut visible = false;
    while !shared.closing.load(Ordering::Acquire) {
        while pending.len() < MAX_PENDING {
            let desired = shared.account_view_visible.load(Ordering::Acquire);
            let command = if desired != visible {
                visible = desired;
                Command::SetAccountView {
                    account_view_visible: visible,
                }
            } else if let Ok(command) = commands.try_recv() {
                command
            } else {
                break;
            };
            *sequence += 1;
            let request = Request {
                id: sequence.to_string(),
                command,
            };
            request.log_submission(&endpoint.session_id);
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
        socket.rearm(writable)?;
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

fn send(socket: &mut RegisteredSocket<'_>, request: &Request) -> Result<()> {
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
    shared.update(|draft| {
        apply_reply(draft, acknowledged.map(|request| request.command), message);
    });
    Ok(())
}

fn apply_reply(draft: &mut crate::ModelDraft<'_>, command: Option<Command>, message: Envelope) {
    let mut cancel_acknowledged = None;
    if matches!(command, Some(Command::SetSenderTrace { .. })) {
        let next = if message.error.is_some() {
            SenderTraceChange::Failed
        } else {
            SenderTraceChange::Idle
        };
        if draft.read().sender_trace_change != next {
            draft.write().sender_trace_change = next;
        }
    } else if let Some(error) = message.error {
        apply_command_error(draft, command, error);
    } else if let Some(Command::CancelSetup { setup_id }) = &command
        && let crate::SetupRequest::Pending {
            id,
            operation_id,
            kind:
                crate::SetupRequestKind::Cancel {
                    acknowledged: false,
                },
        } = &draft.read().setup_request
        && id == setup_id
    {
        let operation_id = operation_id.clone();
        draft.write().setup_request = crate::SetupRequest::Pending {
            id: setup_id.clone(),
            operation_id,
            kind: crate::SetupRequestKind::Cancel { acknowledged: true },
        };
        cancel_acknowledged = Some(setup_id.clone());
    }
    if let Some(snapshot) = message.snapshot {
        if draft.read().accepts_snapshot(&snapshot) {
            draft.write().reconcile(snapshot);
        } else if let Some(setup_id) = cancel_acknowledged
            && draft.read().matches_session(&snapshot)
        {
            // An idempotent CancelSetup reply can echo the current revision after
            // the setup already reached a terminal state or was superseded.
            let model = draft.write();
            model.reconcile_setup_request();
            tracing::debug!(
                event = "desktop.setup_cancel_settled_on_reply",
                session_id = ?model.session_id,
                setup_id = %setup_id,
                revision = model.revision,
                setup_state = model.setup.state_name(),
                pending = model.setup_pending(),
            );
        }
    }
}

fn apply_command_error(
    draft: &mut crate::ModelDraft<'_>,
    command: Option<Command>,
    error: crate::Failure,
) {
    // Only the matching pending request may release its form, even when
    // unrelated refreshes fail or snapshots coalesce.
    match command {
        Some(Command::Create { operation_id, .. } | Command::Repeat { operation_id, .. }) => {
            if draft.read().can_reject(&operation_id, &error) {
                draft.write().reject(&operation_id, error);
            }
        }
        Some(Command::Account { operation_id, .. }) => {
            if matches!(
                &draft.read().account_request,
                crate::AccountRequest::Pending { id } if id == &operation_id
            ) {
                draft.write().account_request = crate::AccountRequest::Failed {
                    id: operation_id,
                    error,
                };
            }
        }
        Some(Command::Configure { setup, .. }) => {
            if matches!(
                &draft.read().setup_request,
                crate::SetupRequest::Pending { id, .. } if id == &setup.id
            ) {
                draft.write().setup_request = crate::SetupRequest::Failed {
                    id: setup.id,
                    error,
                };
            }
        }
        Some(Command::CancelSetup { setup_id }) => {
            if matches!(
                &draft.read().setup_request,
                crate::SetupRequest::Pending { id, .. } if id == &setup_id
            ) {
                draft.write().setup_request = crate::SetupRequest::Failed {
                    id: setup_id,
                    error,
                };
            }
        }
        _ => draft.set_command_error(Some(error.message)),
    }
}
