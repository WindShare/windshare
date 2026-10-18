use super::{Ingress, ResponseBody};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use http_body_util::Full;
use hyper::{Method, Request, Response, StatusCode, body::Bytes, header};
use tokio_tungstenite::tungstenite::handshake::server::create_response_with_body;
use windshare_relay_core::identifiers::ShareId;

pub(super) const HEALTH_PATH: &str = "/healthz";

pub(super) enum Route {
    Reply(Response<ResponseBody>),
    Upgrade {
        share: ShareId,
        response: Response<ResponseBody>,
    },
}

impl Ingress {
    pub(super) fn route<B>(&self, request: &Request<B>) -> Route {
        let path = request.uri().path();
        if path != HEALTH_PATH && path != self.endpoint.path {
            return Route::Reply(reply(StatusCode::NOT_FOUND, "not found\n"));
        }
        if request.method() != Method::GET
            && !(path == HEALTH_PATH && request.method() == Method::HEAD)
        {
            let mut response = reply(StatusCode::METHOD_NOT_ALLOWED, "method not allowed\n");
            let allowed = if path == HEALTH_PATH {
                "GET, HEAD"
            } else {
                "GET"
            };
            response
                .headers_mut()
                .insert(header::ALLOW, allowed.parse().expect("static methods"));
            return Route::Reply(response);
        }
        // Neither endpoint consumes HTTP bodies. Reject them without waiting so
        // body bytes cannot be reinterpreted as frames at the protocol boundary.
        if request.headers().contains_key(header::TRANSFER_ENCODING)
            || request
                .headers()
                .get(header::CONTENT_LENGTH)
                .is_some_and(|value| value != "0")
        {
            return Route::Reply(reply(
                StatusCode::BAD_REQUEST,
                "request body is not supported\n",
            ));
        }
        if path == HEALTH_PATH {
            return Route::Reply(reply(StatusCode::OK, "ok\n"));
        }
        if !self.origins.allows(request.headers()) {
            return Route::Reply(reply(StatusCode::FORBIDDEN, "origin is not allowed\n"));
        }
        let Ok(share) = share_hint(request) else {
            return Route::Reply(reply(
                StatusCode::BAD_REQUEST,
                "invalid share_id routing hint\n",
            ));
        };
        create_response_with_body(request, Full::default).map_or_else(
            |_| {
                Route::Reply(reply(
                    StatusCode::BAD_REQUEST,
                    "invalid WebSocket upgrade\n",
                ))
            },
            |response| Route::Upgrade { share, response },
        )
    }
}

fn share_hint<B>(request: &Request<B>) -> Result<ShareId> {
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

pub(super) fn reply(status: StatusCode, body: &'static str) -> Response<ResponseBody> {
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "text/plain; charset=utf-8")
        .header(header::CONTENT_LENGTH, body.len())
        .header(header::CONNECTION, "close")
        .body(Full::new(Bytes::from_static(body.as_bytes())))
        .expect("static HTTP response")
}

#[cfg(test)]
#[path = "route_tests.rs"]
mod tests;
