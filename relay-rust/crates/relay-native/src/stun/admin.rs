use super::metrics::ListenerStatus;
use anyhow::Result;
use http_body_util::Full;
use hyper::{
    Method, Request, Response, StatusCode, body::Bytes, header, server::conn::http1,
    service::service_fn,
};
use hyper_util::rt::TokioIo;
use std::{
    convert::Infallible,
    fmt::Write,
    sync::{Arc, atomic::Ordering},
    time::Duration,
};
use tokio::io::{AsyncRead, AsyncWrite};

pub(super) const MAX_CONNECTIONS: usize = 16;
const REQUEST_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_HEADER_BYTES: usize = 8 * 1024;
const MAX_HEADERS: usize = 32;
const TEXT_CONTENT_TYPE: &str = "text/plain; charset=utf-8";
const METRICS_CONTENT_TYPE: &str = "text/plain; version=0.0.4";
type Body = Full<Bytes>;
pub(super) type Statuses = Arc<[Arc<ListenerStatus>]>;

pub(super) async fn serve<S>(stream: S, statuses: Statuses) -> Result<()>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    // One deadline bounds reads and writes, including clients that never read
    // their metrics response. Admin requests own no relay admission resources.
    let service = service_fn(move |request| {
        let response = route(&request, &statuses);
        async move { Ok::<_, Infallible>(response) }
    });
    tokio::time::timeout(
        REQUEST_TIMEOUT,
        http1::Builder::new()
            .keep_alive(false)
            .header_read_timeout(None)
            .max_headers(MAX_HEADERS)
            .max_buf_size(MAX_HEADER_BYTES)
            .serve_connection(TokioIo::new(stream), service),
    )
    .await??;
    Ok(())
}

fn route<B>(request: &Request<B>, statuses: &[Arc<ListenerStatus>]) -> Response<Body> {
    let path = request.uri().path();
    let head = request.method() == Method::HEAD;
    let (status, body, content_type) = if !matches!(path, "/healthz" | "/metrics") {
        (
            StatusCode::NOT_FOUND,
            "not found\n".to_owned(),
            TEXT_CONTENT_TYPE,
        )
    } else if request.method() != Method::GET && !head {
        (
            StatusCode::METHOD_NOT_ALLOWED,
            "method not allowed\n".to_owned(),
            TEXT_CONTENT_TYPE,
        )
    } else if request.headers().contains_key(header::TRANSFER_ENCODING)
        || request
            .headers()
            .get(header::CONTENT_LENGTH)
            .is_some_and(|value| value != "0")
    {
        (
            StatusCode::BAD_REQUEST,
            "request body is not supported\n".to_owned(),
            TEXT_CONTENT_TYPE,
        )
    } else if path == "/metrics" {
        (StatusCode::OK, metrics(statuses), METRICS_CONTENT_TYPE)
    } else {
        let mut body = String::new();
        let mut healthy = !statuses.is_empty();
        for listener in statuses {
            let available = listener.healthy();
            healthy &= available;
            writeln!(body, "udp-{} healthy={available}", listener.id).expect("string output");
        }
        (
            if healthy {
                StatusCode::OK
            } else {
                StatusCode::SERVICE_UNAVAILABLE
            },
            body,
            TEXT_CONTENT_TYPE,
        )
    };
    let mut response = Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, content_type)
        .header(header::CONTENT_LENGTH, body.len())
        .header(header::CONNECTION, "close");
    if status == StatusCode::METHOD_NOT_ALLOWED {
        response = response.header(header::ALLOW, "GET, HEAD");
    }
    response
        .body(if head {
            Full::default()
        } else {
            Full::new(Bytes::from(body))
        })
        .expect("bounded admin response")
}

fn metrics(statuses: &[Arc<ListenerStatus>]) -> String {
    let mut output = String::new();
    for listener in statuses {
        let metrics = &listener.metrics;
        for (name, value) in [
            ("received_total", metrics.received.load(Ordering::Relaxed)),
            ("responded_total", metrics.responded.load(Ordering::Relaxed)),
            ("invalid_total", metrics.invalid.load(Ordering::Relaxed)),
            ("limited_total", metrics.limited.load(Ordering::Relaxed)),
            (
                "write_errors_total",
                metrics.write_errors.load(Ordering::Relaxed),
            ),
            (
                "read_errors_total",
                metrics.read_errors.load(Ordering::Relaxed),
            ),
            ("healthy", u64::from(listener.healthy())),
        ] {
            // Labels are configuration indices, never network-controlled IPs.
            writeln!(
                output,
                "windshare_stun_{name}{{listener=\"udp-{}\"}} {value}",
                listener.id
            )
            .expect("string output");
        }
    }
    output
}

#[cfg(test)]
#[path = "admin_tests.rs"]
mod tests;
