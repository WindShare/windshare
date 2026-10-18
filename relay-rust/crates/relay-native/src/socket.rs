use crate::{config::MAX_MESSAGE_BYTES, connection, http::Ingress, registry::Registry};
use anyhow::Result;
use socket2::SockRef;
use std::{sync::Arc, time::Duration};
use tokio::{
    net::TcpStream,
    sync::{OwnedSemaphorePermit, watch},
};
use tokio_tungstenite::{
    WebSocketStream,
    tungstenite::protocol::{Role, WebSocketConfig},
};
use windshare_relay_core::lifecycle::ConnectionRef;

pub async fn serve(
    stream: TcpStream,
    ingress: Arc<Ingress>,
    registry: Arc<Registry>,
    connection: ConnectionRef,
    mut shutdown: watch::Receiver<bool>,
    http_permit: OwnedSemaphorePermit,
) -> Result<()> {
    stream.set_nodelay(true)?;
    let peer = stream.peer_addr()?;
    let Some(upgrade) = (tokio::select! {
        result = ingress.accept(stream, peer, connection) => result?,
        () = connection::stopped(&mut shutdown) => return Ok(()),
    }) else {
        return Ok(());
    };
    drop(http_permit);
    // Default TCP close may retain and transmit queued bytes after the application
    // drops its handles. Abortive close makes joined transport teardown a release fact.
    SockRef::from(&upgrade.stream).set_linger(Some(Duration::ZERO))?;
    let config = WebSocketConfig::default()
        .max_message_size(Some(MAX_MESSAGE_BYTES))
        .max_frame_size(Some(MAX_MESSAGE_BYTES))
        .write_buffer_size(0)
        .max_write_buffer_size(MAX_MESSAGE_BYTES * 2);
    let socket = WebSocketStream::from_partially_read(
        upgrade.stream,
        upgrade.buffered,
        Role::Server,
        Some(config),
    )
    .await;
    let share = upgrade.share;
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
                upgrade.admission,
            ),
        )?
        .await
}
