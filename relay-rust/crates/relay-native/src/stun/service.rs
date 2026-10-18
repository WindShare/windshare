use super::{
    admin::{self, Statuses},
    config::Config,
    metrics::ListenerStatus,
    udp,
};
use anyhow::Result;
use std::{
    future::{Future, pending},
    io,
    net::SocketAddr,
    sync::Arc,
};
use tokio::{
    net::{TcpListener, TcpStream, UdpSocket},
    sync::Semaphore,
    task::JoinSet,
};

struct BoundListener {
    socket: UdpSocket,
    address: SocketAddr,
    status: Arc<ListenerStatus>,
}

/// Binds independently, then owns all UDP and admin tasks until joined shutdown.
pub struct Service {
    config: Config,
    listeners: Vec<BoundListener>,
    statuses: Statuses,
    admin: Option<(TcpListener, SocketAddr)>,
}

impl Service {
    /// Socket failures remain visible in STUN health and never fail relay startup.
    ///
    /// # Errors
    /// Rejects invalid configuration before binding any listeners.
    pub fn bind(config: Config) -> Result<Self> {
        config.validate()?;
        let mut listeners = Vec::new();
        let mut statuses = Vec::new();
        for (id, address) in config.udp_addresses.iter().enumerate() {
            let status = Arc::new(ListenerStatus::new(id));
            match udp::bind(*address).and_then(|socket| Ok((socket.local_addr()?, socket))) {
                Ok((address, socket)) => {
                    tracing::info!(event = "stun_listener_bound", listener_id = id, %address,
                        requests_per_second = config.limits.requests_per_second.get(),
                        source_requests_per_second = config.limits.source_requests_per_second.get(),
                        max_sources = config.limits.max_sources.get());
                    listeners.push(BoundListener {
                        socket,
                        address,
                        status: Arc::clone(&status),
                    });
                }
                Err(error) => {
                    tracing::warn!(event = "stun_listener_unavailable", listener_id = id, %address, %error);
                }
            }
            statuses.push(status);
        }
        let admin = if statuses.is_empty() {
            None
        } else {
            config.admin_address.and_then(bind_admin)
        };
        Ok(Self {
            config,
            listeners,
            statuses: statuses.into(),
            admin,
        })
    }

    #[must_use]
    pub fn udp_addresses(&self) -> Vec<SocketAddr> {
        self.listeners
            .iter()
            .map(|listener| listener.address)
            .collect()
    }

    #[must_use]
    pub fn admin_address(&self) -> Option<SocketAddr> {
        self.admin.as_ref().map(|(_, address)| *address)
    }

    /// Shutdown interrupts pending UDP/admin writes and joins every owned task.
    pub async fn serve(self, shutdown: impl Future<Output = ()>) {
        let Self {
            config,
            listeners,
            statuses,
            mut admin,
        } = self;
        let mut tasks = JoinSet::new();
        for listener in listeners {
            let limits = config.limits;
            tasks.spawn(async move {
                let id = listener.status.id;
                tracing::info!(event = "stun_listener_started", listener_id = id, address = %listener.address);
                if let Err(error) = udp::serve(listener.socket, limits, listener.status).await {
                    tracing::warn!(event = "stun_listener_failed", listener_id = id, %error);
                }
            });
        }
        let slots = Arc::new(Semaphore::new(admin::MAX_CONNECTIONS));
        tokio::pin!(shutdown);
        loop {
            tokio::select! {
                biased;
                () = &mut shutdown => break,
                result = tasks.join_next(), if !tasks.is_empty() => {
                    if let Some(Err(error)) = result {
                        tracing::error!(event = "stun_task_failed", %error);
                    }
                }
                accepted = accept(admin.as_ref()) => match accepted {
                    Ok((stream, peer)) => {
                        let Ok(permit) = Arc::clone(&slots).try_acquire_owned() else {
                            tracing::debug!(event = "stun_admin_limited", %peer);
                            continue;
                        };
                        let statuses = Arc::clone(&statuses);
                        tasks.spawn(async move {
                            let _permit = permit;
                            if let Err(error) = admin::serve(stream, statuses).await {
                                tracing::debug!(event = "stun_admin_request_failed", %peer, %error);
                            }
                        });
                    }
                    Err(error) => {
                        tracing::warn!(event = "stun_admin_failed", %error);
                        admin = None;
                    }
                }
            }
        }
        drop(admin);
        // UDP loops and admin exchanges live in this same set. No supervisor
        // can return while a detached child still owns a socket or source table.
        tasks.shutdown().await;
        tracing::info!(event = "stun_service_stopped");
    }
}

fn bind_admin(address: SocketAddr) -> Option<(TcpListener, SocketAddr)> {
    let bind = || -> io::Result<_> {
        let listener = std::net::TcpListener::bind(address)?;
        listener.set_nonblocking(true)?;
        let address = listener.local_addr()?;
        Ok((TcpListener::from_std(listener)?, address))
    };
    match bind() {
        Ok((listener, bound)) => {
            tracing::info!(event = "stun_admin_started", address = %bound);
            Some((listener, bound))
        }
        Err(error) => {
            tracing::warn!(event = "stun_admin_unavailable", %address, %error);
            None
        }
    }
}

async fn accept(admin: Option<&(TcpListener, SocketAddr)>) -> io::Result<(TcpStream, SocketAddr)> {
    match admin {
        Some((listener, _)) => listener.accept().await,
        None => pending().await,
    }
}

#[cfg(test)]
#[path = "service_tests.rs"]
mod tests;
