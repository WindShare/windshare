use super::{Console, Reply};
use http_body_util::{BodyExt, Full, Limited};
use hyper::{
    Method, Request, Response, StatusCode,
    body::{Bytes, Incoming},
    header,
};
use serde_json::{Value, json};
use windshare_relay_console::{
    MAX_BODY_BYTES, NO_STORE_CACHE_CONTROL, POLICY, asset, asset_cache_control,
    http::{check_mutation_headers, extract_cookie, format_session_cookie},
};

const COOKIE: &str = "windshare_node_admin";
type HttpResponse = Response<Full<Bytes>>;

fn response(status: StatusCode, kind: &str, cache_control: &str, body: Bytes) -> HttpResponse {
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, kind)
        .header(header::CACHE_CONTROL, cache_control)
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

    pub(super) fn render(&self, reply: Reply) -> HttpResponse {
        let mut res = response(
            reply.status,
            "application/json",
            NO_STORE_CACHE_CONTROL,
            Bytes::from(reply.body.to_string()),
        );
        if let Some(secret) = reply.cookie {
            let value = format_session_cookie(
                COOKIE,
                &secret,
                "/api",
                self.origin().starts_with("https://"),
            );
            res.headers_mut()
                .insert(header::SET_COOKIE, value.parse().expect("base64url cookie"));
        }
        res
    }

    pub(crate) async fn handle(
        &self,
        request: Request<Incoming>,
        node: &crate::node::Runtime,
    ) -> HttpResponse {
        let (parts, body) = request.into_parts();
        let path = parts.uri.path();
        if parts.method == Method::GET
            && let Some((body, kind)) = asset(path)
        {
            return response(
                StatusCode::OK,
                kind,
                asset_cache_control(path),
                Bytes::from_static(body.as_bytes()),
            );
        }
        let h = &parts.headers;
        let value = if parts.method == Method::POST {
            let origin = h.get(header::ORIGIN).and_then(|v| v.to_str().ok());
            let sec_fetch_site = h.get("sec-fetch-site").and_then(|v| v.to_str().ok());
            let content_type = h.get(header::CONTENT_TYPE).and_then(|v| v.to_str().ok());
            if let Err(error) =
                check_mutation_headers(path, self.origin(), origin, sec_fetch_site, content_type)
            {
                return self.render(Reply::from_console_error(error));
            }
            let Ok(data) = Limited::new(body, MAX_BODY_BYTES).collect().await else {
                return self.render(Reply::error(
                    StatusCode::PAYLOAD_TOO_LARGE,
                    "body_too_large",
                ));
            };
            let Ok(value) = serde_json::from_slice::<Value>(&data.to_bytes()) else {
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
            .and_then(|v| extract_cookie(v, COOKIE))
            .unwrap_or("")
            .to_owned();
        let console = self.clone();
        let node = node.clone();
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
                Some(&node),
            )
        })
        .await
        .map_err(anyhow::Error::from)
        .and_then(std::convert::identity);
        match result {
            Ok(reply) => self.render(reply),
            Err(reason) => {
                tracing::warn!(event = "node_console_failure", %reason);
                self.render(Reply::error(
                    StatusCode::SERVICE_UNAVAILABLE,
                    "temporarily_unavailable",
                ))
            }
        }
    }
}
