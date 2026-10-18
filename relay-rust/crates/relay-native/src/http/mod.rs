mod origin;
mod route;

pub use origin::OriginPolicy;

use crate::{
    config::{Endpoint, HTTP_REQUEST_TIMEOUT},
    connection::{Admission, Capacity},
    environment::{Environment, NativeEnvironment},
};
use anyhow::Result;
use http_body_util::Full;
use hyper::{
    Request, Response, StatusCode,
    body::{Bytes, Incoming},
    server::conn::http1,
    service::Service,
};
use hyper_util::rt::TokioIo;
use route::{Route, reply};
use std::{
    cell::RefCell,
    convert::Infallible,
    future::{Ready, ready},
    sync::Arc,
};
use tokio::io::{AsyncRead, AsyncWrite, AsyncWriteExt};
use windshare_relay_core::{identifiers::ShareId, lifecycle::ConnectionRef};

const MAX_HTTP_HEADER_BYTES: usize = 16 * 1024;
const MAX_HTTP_HEADERS: usize = 64;
type ResponseBody = Full<Bytes>;

pub struct Ingress {
    endpoint: Endpoint,
    origins: OriginPolicy,
    capacity: Arc<Capacity>,
    environment: NativeEnvironment,
}

pub struct Upgrade<S> {
    pub stream: S,
    pub buffered: Vec<u8>,
    pub share: ShareId,
    pub admission: Arc<Admission>,
}

impl Ingress {
    pub const fn new(
        endpoint: Endpoint,
        origins: OriginPolicy,
        capacity: Arc<Capacity>,
        environment: NativeEnvironment,
    ) -> Self {
        Self {
            endpoint,
            origins,
            capacity,
            environment,
        }
    }

    pub async fn accept<S>(
        &self,
        stream: S,
        connection: ConnectionRef,
    ) -> Result<Option<Upgrade<S>>>
    where
        S: AsyncRead + AsyncWrite + Unpin,
    {
        // The absolute budget covers both request reads and response writes.
        // A slow reader cannot keep HTTP or upgrade admission alive indefinitely.
        tokio::time::timeout(HTTP_REQUEST_TIMEOUT, self.exchange(stream, connection)).await?
    }

    async fn exchange<S>(&self, stream: S, connection: ConnectionRef) -> Result<Option<Upgrade<S>>>
    where
        S: AsyncRead + AsyncWrite + Unpin,
    {
        let service = RequestService {
            ingress: self,
            connection,
            upgrade: RefCell::new(None),
        };
        // One bounded HTTP exchange precedes ownership by the connection runtime.
        // Taking the transport back avoids detached upgrade tasks and preserves
        // every byte read ahead of the 101 response under the existing supervisor.
        let parts = http1::Builder::new()
            // Ordinary replies carry Connection: close. Disabling persistence
            // globally would also strip Connection: Upgrade from the 101 reply.
            .keep_alive(true)
            .header_read_timeout(None)
            .max_headers(MAX_HTTP_HEADERS)
            .max_buf_size(MAX_HTTP_HEADER_BYTES)
            .serve_connection(TokioIo::new(stream), service)
            .without_shutdown()
            .await?;
        let mut stream = parts.io.into_inner();
        if let Some((share, admission)) = parts.service.upgrade.into_inner() {
            return Ok(Some(Upgrade {
                stream,
                buffered: parts.read_buf.to_vec(),
                share,
                admission,
            }));
        }
        stream.shutdown().await?;
        Ok(None)
    }
}

struct RequestService<'a> {
    ingress: &'a Ingress,
    connection: ConnectionRef,
    upgrade: RefCell<Option<(ShareId, Arc<Admission>)>>,
}

impl Service<Request<Incoming>> for RequestService<'_> {
    type Response = Response<ResponseBody>;
    type Error = Infallible;
    type Future = Ready<Result<Self::Response, Self::Error>>;

    fn call(&self, request: Request<Incoming>) -> Self::Future {
        let response = match self.ingress.route(&request) {
            Route::Reply(response) => response,
            Route::Upgrade { share, response } => {
                match self.ingress.capacity.accept(self.ingress.environment.now()) {
                    Ok(admission) => {
                        *self.upgrade.borrow_mut() = Some((share, admission));
                        response
                    }
                    Err(error) => {
                        tracing::debug!(event = "control_connection_refused", connection = ?self.connection, %error);
                        reply(
                            StatusCode::SERVICE_UNAVAILABLE,
                            "relay connection capacity exceeded\n",
                        )
                    }
                }
            }
        };
        tracing::debug!(event = "http_request", connection = ?self.connection,
            method = %request.method(), path = request.uri().path(),
            origin = ?request.headers().get_all(hyper::header::ORIGIN), status = response.status().as_u16());
        ready(Ok(response))
    }
}

#[cfg(test)]
#[path = "ingress_tests.rs"]
mod tests;
