use crate::{
    config::{Endpoint, MAX_MESSAGE_BYTES, SOCKET_TIMEOUT},
    connection,
    registry::Registry,
};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use socket2::SockRef;
use std::{sync::Arc, time::Duration};
use tokio::{net::TcpStream, sync::watch, time::timeout};
use tokio_tungstenite::{
    accept_hdr_async_with_config,
    tungstenite::{
        handshake::server::{Request, Response},
        protocol::WebSocketConfig,
    },
};
use windshare_relay_core::{identifiers::ShareId, lifecycle::ConnectionRef};

#[expect(
    clippy::result_large_err,
    reason = "Tungstenite requires the HTTP upgrade response by value"
)]
pub async fn serve(
    stream: TcpStream,
    endpoint: Endpoint,
    registry: Arc<Registry>,
    connection: ConnectionRef,
    mut shutdown: watch::Receiver<bool>,
    admission: Arc<connection::Admission>,
) -> Result<()> {
    stream.set_nodelay(true)?;
    // Default TCP close may retain and transmit queued bytes after the application
    // drops its handles. Abortive close makes joined transport teardown a release fact.
    SockRef::from(&stream).set_linger(Some(Duration::ZERO))?;
    let mut routed = None;
    let config = WebSocketConfig::default()
        .max_message_size(Some(MAX_MESSAGE_BYTES))
        .max_frame_size(Some(MAX_MESSAGE_BYTES))
        .write_buffer_size(0)
        .max_write_buffer_size(MAX_MESSAGE_BYTES * 2);
    let upgrade = timeout(
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
    );
    let socket = tokio::select! {
        result = upgrade => result??,
        () = connection::stopped(&mut shutdown) => return Ok(()),
    };
    let share = routed.ok_or_else(|| anyhow!("missing upgrade route"))?;
    let handle = registry.attach(share)?;
    tracing::debug!(event = "socket_connected", ?share, ?connection);
    handle
        .generation
        .admit(
            &handle.tx,
            connection::run(
                socket,
                handle.tx.clone(),
                connection,
                shutdown,
                handle.generation.clone(),
                admission,
            ),
        )?
        .await
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

#[cfg(test)]
#[path = "socket_tests.rs"]
mod tests;
