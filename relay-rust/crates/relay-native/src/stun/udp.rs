use super::{
    config::Limits,
    limits::Limiter,
    metrics::{ListenerStatus, Serving},
    protocol::binding_response,
};
use socket2::{Domain, Protocol, Socket, Type};
use std::{
    future::Future,
    io,
    net::SocketAddr,
    sync::{Arc, atomic::Ordering},
    time::Duration,
};
use tokio::{
    net::UdpSocket,
    time::{Instant, timeout},
};

// A full UDP receive buffer prevents both truncation and Windows WSAEMSGSIZE
// from turning an oversized datagram into a failed listener. Protocol work still
// rejects anything above MAX_DATAGRAM_BYTES; only one buffer exists per socket.
const RECEIVE_BUFFER_BYTES: usize = 65_535;
const WRITE_TIMEOUT: Duration = Duration::from_secs(1);

pub(super) trait Datagram: Send {
    fn receive(
        &mut self,
        buffer: &mut [u8],
    ) -> impl Future<Output = io::Result<(usize, SocketAddr)>> + Send;
    fn send(
        &mut self,
        data: &[u8],
        remote: SocketAddr,
    ) -> impl Future<Output = io::Result<usize>> + Send;
}

impl Datagram for UdpSocket {
    async fn receive(&mut self, buffer: &mut [u8]) -> io::Result<(usize, SocketAddr)> {
        self.recv_from(buffer).await
    }

    async fn send(&mut self, data: &[u8], remote: SocketAddr) -> io::Result<usize> {
        self.send_to(data, remote).await
    }
}

pub(super) fn bind(address: SocketAddr) -> io::Result<UdpSocket> {
    let socket = Socket::new(
        Domain::for_address(address),
        Type::DGRAM,
        Some(Protocol::UDP),
    )?;
    if address.is_ipv6() {
        // Explicit families allow v4 and v6 listeners to share the same port on
        // every platform without one wildcard listener shadowing the other.
        socket.set_only_v6(true)?;
    }
    socket.set_nonblocking(true)?;
    socket.bind(&address.into())?;
    UdpSocket::from_std(socket.into())
}

pub(super) async fn serve(
    mut socket: impl Datagram,
    limits: Limits,
    status: Arc<ListenerStatus>,
) -> io::Result<()> {
    let _serving = Serving::begin(&status);
    let mut limiter = Limiter::new(limits, Instant::now());
    let mut buffer = vec![0; RECEIVE_BUFFER_BYTES];
    loop {
        let (length, remote) = match socket.receive(&mut buffer).await {
            Ok(received) => received,
            Err(error) => {
                status.metrics.read_errors.fetch_add(1, Ordering::Relaxed);
                // Unconnected UDP can report an ICMP error from an earlier send.
                // Such a peer failure is not evidence that the listener is dead.
                if matches!(
                    error.kind(),
                    io::ErrorKind::ConnectionReset
                        | io::ErrorKind::ConnectionRefused
                        | io::ErrorKind::Interrupted
                ) {
                    tracing::debug!(event = "stun_receive_retry", listener_id = status.id, %error);
                    tokio::task::yield_now().await;
                    continue;
                }
                return Err(error);
            }
        };
        status.metrics.received.fetch_add(1, Ordering::Relaxed);
        if let Err(reason) = limiter.admit(remote.ip(), Instant::now()) {
            status.metrics.limited.fetch_add(1, Ordering::Relaxed);
            tracing::debug!(event = "stun_limited", listener_id = status.id, %remote, ?reason);
            continue;
        }
        let response = match binding_response(&buffer[..length], remote) {
            Ok(response) => response,
            Err(reason) => {
                status.metrics.invalid.fetch_add(1, Ordering::Relaxed);
                tracing::debug!(event = "stun_invalid", listener_id = status.id, %remote, length, ?reason);
                continue;
            }
        };
        match timeout(WRITE_TIMEOUT, socket.send(&response, remote)).await {
            Ok(Ok(written)) if written == response.len() => {
                status.metrics.responded.fetch_add(1, Ordering::Relaxed);
                tracing::trace!(event = "stun_responded", listener_id = status.id, %remote,
                    transaction = ?&buffer[8..20], response_type = ?&response[..2]);
            }
            result => {
                status.metrics.write_errors.fetch_add(1, Ordering::Relaxed);
                tracing::debug!(event = "stun_write_failed", listener_id = status.id, %remote, ?result);
            }
        }
    }
}

#[cfg(test)]
#[path = "udp_tests.rs"]
mod tests;
