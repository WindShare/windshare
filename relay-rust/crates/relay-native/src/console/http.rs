use super::{Console, Reply, claimant};
use http_body_util::{BodyExt, Full, Limited};
use hyper::{
    Method, Request, Response, StatusCode,
    body::{Bytes, Incoming},
    header,
};
use serde_json::json;
use windshare_relay_console::{MAX_BODY_BYTES, POLICY, asset};

const COOKIE: &str = "windshare_node_admin";
type HttpResponse = Response<Full<Bytes>>;

fn response(status: StatusCode, kind: &str, body: Bytes) -> HttpResponse {
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, kind)
        .header(header::CACHE_CONTROL, "no-store")
        .header(header::CONTENT_SECURITY_POLICY, POLICY)
        .header(header::X_CONTENT_TYPE_OPTIONS, "nosniff")
        .header(header::REFERRER_POLICY, "no-referrer")
        .header(header::CONNECTION, "close")
        .body(Full::new(body))
        .expect("static response headers")
}

impl Console {
    pub(crate) fn handles(path: &str) -> bool {
        asset(path).is_some() || path.starts_with("/api/")
    }

    fn render(&self, reply: Reply) -> HttpResponse {
        let mut res = response(
            reply.status,
            "application/json",
            Bytes::from(reply.body.to_string()),
        );
        if let Some(secret) = reply.cookie {
            let secure = if self.origin().starts_with("https://") {
                "; Secure"
            } else {
                ""
            };
            let age = if secret.is_empty() { "; Max-Age=0" } else { "" };
            let value =
                format!("{COOKIE}={secret}; HttpOnly; SameSite=Strict; Path=/api{secure}{age}");
            res.headers_mut()
                .insert(header::SET_COOKIE, value.parse().expect("base64url cookie"));
        }
        res
    }

    pub(crate) async fn handle(&self, request: Request<Incoming>) -> HttpResponse {
        let (parts, body) = request.into_parts();
        let path = parts.uri.path();
        if parts.method == Method::GET
            && let Some((body, kind)) = asset(path)
        {
            return response(StatusCode::OK, kind, Bytes::from_static(body.as_bytes()));
        }
        let h = &parts.headers;
        let value = if parts.method == Method::POST {
            let origin = h.get(header::ORIGIN).and_then(|v| v.to_str().ok());
            if origin.is_some_and(|v| v != self.origin())
                || (!claimant(path) && origin != Some(self.origin()))
                || h.get("sec-fetch-site").is_some_and(|v| v == "cross-site")
            {
                return self.render(Reply::error(StatusCode::FORBIDDEN, "origin_required"));
            }
            if h.get(header::CONTENT_TYPE)
                .and_then(|v| v.to_str().ok())
                .is_none_or(|v| v.split(';').next() != Some("application/json"))
            {
                return self.render(Reply::error(
                    StatusCode::UNSUPPORTED_MEDIA_TYPE,
                    "json_required",
                ));
            }
            let Ok(data) = Limited::new(body, MAX_BODY_BYTES).collect().await else {
                return self.render(Reply::error(
                    StatusCode::PAYLOAD_TOO_LARGE,
                    "body_too_large",
                ));
            };
            let Ok(value) = serde_json::from_slice(&data.to_bytes()) else {
                return self.render(Reply::error(StatusCode::BAD_REQUEST, "invalid_json"));
            };
            value
        } else if parts.method == Method::GET {
            json!({})
        } else {
            return self.render(Reply::error(
                StatusCode::METHOD_NOT_ALLOWED,
                "method_not_allowed",
            ));
        };
        let session = h
            .get(header::COOKIE)
            .and_then(|v| v.to_str().ok())
            .and_then(|v| {
                v.split(';').find_map(|part| {
                    part.trim()
                        .split_once('=')
                        .filter(|(name, _)| *name == COOKIE)
                        .map(|(_, s)| s)
                })
            })
            .unwrap_or("")
            .to_owned();
        let console = self.clone();
        let path_and_query = parts
            .uri
            .path_and_query()
            .map_or(path, |value| value.as_str())
            .to_owned();
        let result = tokio::task::spawn_blocking(move || {
            console.dispatch(
                &parts.method,
                &path_and_query,
                &value,
                &session,
                u64::try_from(
                    std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)?
                        .as_millis(),
                )?,
            )
        })
        .await
        .map_err(anyhow::Error::from)
        .and_then(std::convert::identity);
        match result {
            Ok(reply) => self.render(reply),
            Err(reason) => {
                tracing::warn!(event="node_console_failure",%reason);
                self.render(Reply::error(
                    StatusCode::SERVICE_UNAVAILABLE,
                    "temporarily_unavailable",
                ))
            }
        }
    }
}
