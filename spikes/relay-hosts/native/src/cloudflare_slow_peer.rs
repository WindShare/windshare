//! A real TLS client whose OS receive window is bounded while the cloud probe resets.
use futures_util::StreamExt;
use serde_json::{Value, json};
use socket2::SockRef;
use std::io::{self, Write};
use std::time::Duration;
use tokio::net::TcpStream;
use tokio_tungstenite::{
    MaybeTlsStream, client_async_tls_with_config,
    tungstenite::{Message, client::IntoClientRequest},
};

const PAUSED_RECEIVE_BYTES: usize = 8 * 1024;
const DRAIN_RECEIVE_BYTES: usize = 2 * 1024 * 1024;
const FRAME_BYTES: usize = 65_536;
const DRAIN_TIMEOUT: Duration = Duration::from_secs(20);

fn emit(value: Value) {
    println!("{value}");
    io::stdout().flush().unwrap();
}

fn read_command() -> String {
    let mut line = String::new();
    io::stdin().read_line(&mut line).unwrap();
    line
}

fn tcp(stream: &MaybeTlsStream<TcpStream>) -> &TcpStream {
    match stream {
        MaybeTlsStream::Plain(stream) => stream,
        MaybeTlsStream::Rustls(stream) => stream.get_ref().0,
        _ => panic!("unexpected TLS backend"),
    }
}

#[tokio::main(flavor = "current_thread")]
async fn main() {
    rustls::crypto::ring::default_provider()
        .install_default()
        .unwrap();
    // Credentials arrive over the owned stdin pipe, never in process arguments or output.
    let input: Value = serde_json::from_str(&read_command()).unwrap();
    let mut request = input["url"]
        .as_str()
        .unwrap()
        .into_client_request()
        .unwrap();
    assert_eq!(request.uri().scheme_str(), Some("wss"));
    request.headers_mut().insert(
        "Authorization",
        format!("Bearer {}", input["token"].as_str().unwrap())
            .parse()
            .unwrap(),
    );
    let host = request.uri().host().unwrap().to_owned();
    let port = request.uri().port_u16().unwrap_or(443);
    let stream = TcpStream::connect((host.as_str(), port)).await.unwrap();
    stream.set_nodelay(true).unwrap();
    // Set before any burst, after TCP negotiated its window scale. Resuming can then
    // enlarge the window so observing the terminal frame does not become a long test.
    SockRef::from(&stream)
        .set_recv_buffer_size(PAUSED_RECEIVE_BYTES)
        .unwrap();
    let (mut socket, _) = client_async_tls_with_config(request, stream, None, None)
        .await
        .unwrap();
    emit(
        json!({"event": "ready", "receive_buffer_bytes": SockRef::from(tcp(socket.get_ref())).recv_buffer_size().unwrap()}),
    );

    // Intentionally do not poll the socket while paused. Only the parent can resume it.
    assert_eq!(read_command().trim(), "resume");
    SockRef::from(tcp(socket.get_ref()))
        .set_recv_buffer_size(DRAIN_RECEIVE_BYTES)
        .unwrap();
    let mut frames = 0;
    let drain = async {
        while let Some(message) = socket.next().await {
            match message {
                Ok(Message::Binary(bytes)) => {
                    assert_eq!(bytes.len(), FRAME_BYTES);
                    assert_eq!(u64::from_be_bytes(bytes[..8].try_into().unwrap()), frames);
                    assert!(bytes[8..].iter().all(|byte| *byte == 0x5a));
                    frames += 1;
                }
                Ok(Message::Close(close)) => {
                    return json!({"kind": "close", "detail": close.map(|c| json!({"code": u16::from(c.code), "reason": c.reason.to_string()}))});
                }
                Err(error) => return json!({"kind": "error", "detail": error.to_string()}),
                _ => {}
            }
        }
        json!({"kind": "eof"})
    };
    let terminal = match tokio::time::timeout(DRAIN_TIMEOUT, drain).await {
        Ok(value) => value,
        Err(_) => json!({"kind": "timeout"}),
    };
    emit(json!({"event": "terminal", "frames": frames, "terminal": terminal}));
}
