use crate::{
    actor::Event,
    config::{Endpoint, MAX_CONTROL_BYTES, SOCKET_TIMEOUT, WRITE_QUEUE_CAPACITY},
    registry::{Handle, Registry},
};
use anyhow::{Result, anyhow, bail, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use futures_util::{SinkExt, StreamExt};
use std::sync::Arc;
use tokio::{net::TcpStream, sync::mpsc, time::timeout};
use tokio_tungstenite::{
    WebSocketStream, accept_hdr_async_with_config,
    tungstenite::{
        Message,
        handshake::server::{Request, Response},
        protocol::WebSocketConfig,
    },
};
use windshare_relay_core::{
    error_frame::ErrorFrame, identifiers::ShareId, lifecycle::ConnectionRef,
};

#[expect(
    clippy::result_large_err,
    reason = "Tungstenite requires the HTTP upgrade response by value"
)]
pub async fn serve(
    stream: TcpStream,
    endpoint: Endpoint,
    registry: Arc<Registry>,
    connection: ConnectionRef,
) -> Result<()> {
    stream.set_nodelay(true)?;
    let mut routed = None;
    let config = WebSocketConfig::default()
        .max_message_size(Some(MAX_CONTROL_BYTES))
        .max_frame_size(Some(MAX_CONTROL_BYTES))
        .write_buffer_size(0)
        .max_write_buffer_size(MAX_CONTROL_BYTES * 2);
    let mut socket = timeout(
        SOCKET_TIMEOUT,
        accept_hdr_async_with_config(
            stream,
            |request: &Request, response: Response| match route(request, &endpoint) {
                Ok(share) => {
                    routed = Some(share);
                    Ok(response)
                }
                Err(error) => Err(Response::builder()
                    .status(400)
                    .body(Some(error.to_string()))
                    .expect("error response")),
            },
            Some(config),
        ),
    )
    .await??;
    let share = routed.ok_or_else(|| anyhow!("missing upgrade route"))?;
    let handle = registry.attach(share)?;
    let (writer, mut outgoing) = mpsc::channel(WRITE_QUEUE_CAPACITY);
    handle.tx.send(Event::Connected(connection, writer)).await?;
    tracing::debug!(event = "socket_connected", ?share, ?connection);
    let result = pump(&mut socket, &mut outgoing, &handle, connection).await;
    // A close request is not a release receipt. Drop the actual transport and
    // application-owned messages before reporting the generation's release.
    drop(socket);
    drop(outgoing);
    let _ = handle.tx.send(Event::Closed(connection)).await;
    result
}

fn route(request: &Request, endpoint: &Endpoint) -> Result<ShareId> {
    ensure!(
        request.uri().path() == endpoint.path,
        "unknown WebSocket path"
    );
    let mut share = None;
    for (key, value) in url::form_urlencoded::parse(request.uri().query().unwrap_or("").as_bytes())
    {
        ensure!(
            key == "share_id" && share.is_none(),
            "expected one share_id routing hint"
        );
        let bytes = URL_SAFE_NO_PAD.decode(value.as_bytes())?;
        ensure!(
            URL_SAFE_NO_PAD.encode(&bytes) == value,
            "noncanonical share_id"
        );
        share = ShareId::new(bytes.try_into().map_err(|_| anyhow!("share_id width"))?);
    }
    share.ok_or_else(|| anyhow!("missing share_id routing hint"))
}

async fn pump(
    socket: &mut WebSocketStream<TcpStream>,
    outgoing: &mut mpsc::Receiver<Vec<u8>>,
    handle: &Handle,
    connection: ConnectionRef,
) -> Result<()> {
    loop {
        tokio::select! {
            // A refusal or STOPPED queued immediately before Close must reach the
            // socket before the producer's channel closure terminates its writer.
            biased;
            frame = outgoing.recv() => {
                let Some(frame) = frame else { return Ok(()); };
                timeout(SOCKET_TIMEOUT, socket.send(Message::Binary(frame.into()))).await??;
            }
            message = socket.next() => {
                match message.transpose()? {
                    Some(Message::Binary(bytes)) => {
                        if !supported_control(&bytes) {
                            timeout(SOCKET_TIMEOUT, socket.send(Message::Binary(
                                ErrorFrame::UnsupportedMode.encode().to_vec().into(),
                            ))).await??;
                            bail!("JOIN and payload forwarding are not enabled in this host");
                        }
                        handle.tx.send(Event::Control(connection, bytes.to_vec())).await?;
                    }
                    Some(Message::Ping(_)) => {
                        timeout(SOCKET_TIMEOUT, socket.flush()).await??;
                    }
                    Some(Message::Pong(_)) => {}
                    Some(Message::Close(_)) | None => return Ok(()),
                    Some(_) => bail!("expected a binary control frame"),
                }
            }
        }
    }
}

fn supported_control(bytes: &[u8]) -> bool {
    // The kernel still validates all layouts, roles and authentication. This
    // capability boundary prevents partially implemented delivery from admission.
    matches!(
        bytes.get(..4),
        Some(b"WS2R" | b"WS2P" | b"WS2U" | b"WS2T" | b"WS2X" | b"WS2V" | b"WS2H")
    )
}

#[cfg(test)]
#[path = "socket_tests.rs"]
mod tests;
