mod origin;
mod route;
mod source;

pub use origin::OriginPolicy;
pub use source::SourcePolicy;

use crate::{
    config::{Endpoint, HTTP_REQUEST_TIMEOUT},
    connection::{Admission, Capacity},
    environment::{Environment, NativeEnvironment},
    node::access::Bearer,
};
use anyhow::Result;
use http_body_util::Full;
use hyper::{
    Request, Response, StatusCode,
    body::{Bytes, Incoming},
    server::conn::http1,
    service::service_fn,
};
use hyper_util::rt::TokioIo;
use route::{Route, reply};
use std::{
    convert::Infallible,
    net::SocketAddr,
    sync::{Arc, Mutex},
};
use tokio::io::{AsyncRead, AsyncWrite, AsyncWriteExt};
use windshare_relay_core::{identifiers::ShareId, lifecycle::ConnectionRef};

const MAX_HTTP_HEADER_BYTES: usize = 16 * 1024;
const MAX_HTTP_HEADERS: usize = 64;
type ResponseBody = Full<Bytes>;

pub struct Ingress {
    pub(crate) discovery: Option<crate::discovery::Discovery>,
    endpoint: Endpoint,
    origins: OriginPolicy,
    sources: SourcePolicy,
    capacity: Arc<Capacity>,
    environment: NativeEnvironment,
    pub(crate) node: crate::node::Runtime,
}

pub struct Upgrade<S> {
    pub stream: S,
    pub buffered: Vec<u8>,
    pub share: ShareId,
    pub admission: Arc<Admission>,
}

impl Ingress {
    pub fn new(
        endpoint: Endpoint,
        origins: OriginPolicy,
        sources: SourcePolicy,
        capacity: Arc<Capacity>,
        environment: NativeEnvironment,
    ) -> Self {
        Self {
            discovery: None,
            endpoint,
            origins,
            sources,
            capacity,
            environment,
            node: crate::node::Runtime::default(),
        }
    }

    pub(crate) fn with_node(mut self, node: crate::node::Runtime) -> Self {
        let _ = self.capacity.access.set(node.access.clone());
        self.node = node;
        self
    }

    pub async fn accept<S>(
        &self,
        stream: S,
        peer: SocketAddr,
        connection: ConnectionRef,
    ) -> Result<Option<Upgrade<S>>>
    where
        S: AsyncRead + AsyncWrite + Unpin,
    {
        // The absolute budget covers both request reads and response writes.
        // A slow reader cannot keep HTTP or upgrade admission alive indefinitely.
        tokio::time::timeout(
            HTTP_REQUEST_TIMEOUT,
            self.exchange(stream, peer, connection),
        )
        .await?
    }

    async fn exchange<S>(
        &self,
        stream: S,
        peer: SocketAddr,
        connection: ConnectionRef,
    ) -> Result<Option<Upgrade<S>>>
    where
        S: AsyncRead + AsyncWrite + Unpin,
    {
        let service = RequestService {
            ingress: self,
            peer,
            connection,
            upgrade: Mutex::new(None),
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
            .serve_connection(
                TokioIo::new(stream),
                service_fn(|request| service.handle(request)),
            )
            .without_shutdown()
            .await?;
        let mut stream = parts.io.into_inner();
        let buffered = parts.read_buf;
        if let Some((share, admission)) = service
            .upgrade
            .into_inner()
            .expect("HTTP exchange finished")
        {
            return Ok(Some(Upgrade {
                stream,
                buffered: buffered.to_vec(),
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
    peer: SocketAddr,
    connection: ConnectionRef,
    upgrade: Mutex<Option<(ShareId, Arc<Admission>)>>,
}

impl RequestService<'_> {
    async fn upgrade(
        &self,
        request: &Request<Incoming>,
        share: ShareId,
        response: Response<ResponseBody>,
    ) -> Response<ResponseBody> {
        let source = match self
            .ingress
            .sources
            .resolve(self.peer.ip(), request.headers())
        {
            Ok(source) => source,
            Err(error) => {
                tracing::debug!(event = "source_identity_refused", connection = ?self.connection,
                    peer = %self.peer, %error);
                return reply(StatusCode::BAD_REQUEST, "invalid proxy source identity\n");
            }
        };
        let mut bearer = match request
            .headers()
            .get_all(hyper::header::AUTHORIZATION)
            .iter()
            .collect::<Vec<_>>()
            .as_slice()
        {
            [] => None,
            [header] => match header
                .to_str()
                .ok()
                .and_then(|value| value.strip_prefix("Bearer "))
            {
                Some(value)
                    if !value.is_empty()
                        && value.len() <= 4096
                        && value.bytes().all(|byte| (b'!'..=b'~').contains(&byte)) =>
                {
                    Some(Bearer::new(value))
                }
                _ => return reply(StatusCode::BAD_REQUEST, "invalid authorization header\n"),
            },
            _ => return reply(StatusCode::BAD_REQUEST, "duplicate authorization header\n"),
        };
        match self
            .ingress
            .node
            .public_budget_refuses(share, bearer.as_ref())
            .await
        {
            Ok(false) => {}
            Ok(true) => {
                tracing::debug!(event = "public_relay_admission_refused", ?share, connection = ?self.connection);
                return route::unavailable("public daily relay budget exhausted\n");
            }
            Err(error) => {
                tracing::warn!(event = "public_relay_attribution_unavailable", ?share, connection = ?self.connection, %error);
                return route::unavailable("relay attribution unavailable\n");
            }
        }
        self.ingress
            .capacity
            .accept(
                source,
                self.connection,
                self.ingress.environment.now(),
                self.ingress.node.access.admission_priority(
                    share,
                    bearer.as_mut(),
                    self.ingress.environment.now().0 / 1000,
                ),
            )
            .map_or_else(
                |_| {
                    self.ingress.node.metrics.admission_refused();
                    reply(
                        StatusCode::SERVICE_UNAVAILABLE,
                        "relay connection capacity exceeded\n",
                    )
                },
                |admission| {
                    if let Some(bearer) = bearer {
                        admission.set_bearer(bearer);
                    }
                    *self.upgrade.lock().expect("HTTP upgrade") = Some((share, admission));
                    response
                },
            )
    }
}

impl RequestService<'_> {
    async fn handle(
        &self,
        request: Request<Incoming>,
    ) -> Result<Response<ResponseBody>, Infallible> {
        let path = request.uri().path();
        if path == crate::discovery::PATH
            && let Some(discovery) = &self.ingress.discovery
        {
            let Ok(source) = self
                .ingress
                .sources
                .resolve(self.peer.ip(), request.headers())
            else {
                return Ok(reply(StatusCode::BAD_REQUEST, "invalid source\n"));
            };
            return Ok(discovery
                .handle(request, source, &self.ingress.node, &self.ingress.endpoint)
                .await);
        }
        // A configured relay mount can share the console's /api/ prefix.
        if let Some(console) = self.ingress.node.console.get()
            && path != self.ingress.endpoint.path
            && crate::console::Console::handles(path)
        {
            return Ok(console.handle(request, &self.ingress.node).await);
        }
        let response = match self.ingress.route(&request) {
            Route::Reply(response) => response,
            Route::Upgrade { share, response } => self.upgrade(&request, share, response).await,
        };
        tracing::debug!(event = "http_request", connection = ?self.connection, peer = %self.peer,
            method = %request.method(), path,
            origin = ?request.headers().get_all(hyper::header::ORIGIN), status = response.status().as_u16());
        Ok(response)
    }
}

#[cfg(test)]
#[path = "ingress_tests.rs"]
mod tests;
