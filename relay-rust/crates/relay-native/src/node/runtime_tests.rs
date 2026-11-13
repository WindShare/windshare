use super::*;
use crate::{ConnectionLimits, Endpoint, OriginPolicy, Server, SourcePolicy};
use futures_util::{SinkExt, StreamExt};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
};

fn runtime(bytes: u64) -> (tempfile::TempDir, Runtime) {
    let directory = tempfile::tempdir().unwrap();
    let endpoint = Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let store = Database::open(&directory.path().join("node.sqlite"), endpoint.identity).unwrap();
    let settings = Settings {
        services: Services::default(),
        relay_budget: Some(MonthlyBudget {
            bytes,
            reset_day_utc: 1,
        }),
    };
    (directory, Runtime::new(settings, store).unwrap())
}

#[tokio::test]
async fn every_writer_spends_one_shared_budget_including_control_bytes() {
    let (_directory, node) = runtime(10);
    let (first, mut first_reader) = tokio::io::duplex(32);
    let (second, mut second_reader) = tokio::io::duplex(32);
    let mut first = Metered::new(first, node.clone(), node.epoch());
    let mut second = Metered::new(second, node.clone(), node.epoch());
    first.write_all(b"control").await.unwrap();
    assert_eq!(second.write(b"payload").await.unwrap(), 3);
    assert!(first.write_all(b"overflow").await.is_err());
    assert!(!node.available());
    let mut control = [0; 7];
    first_reader.read_exact(&mut control).await.unwrap();
    assert_eq!(&control, b"control");
    let mut payload = [0; 3];
    second_reader.read_exact(&mut payload).await.unwrap();
    assert_eq!(&payload, b"pay");
}

#[tokio::test]
async fn service_revocation_is_not_lost_when_immediately_reenabled() {
    let node = Runtime::default();
    let epoch = node.epoch();
    node.update(
        Settings {
            services: Services {
                relay: false,
                stun: true,
            },
            relay_budget: None,
        },
        now(),
    )
    .unwrap();
    node.update(Settings::default(), now()).unwrap();
    tokio::time::timeout(Duration::from_secs(1), node.revoked(epoch))
        .await
        .unwrap();
    assert!(node.available());
    let (stream, _) = tokio::io::duplex(32);
    let mut old = Metered::new(stream, node, epoch);
    assert!(old.write_all(b"old-session").await.is_err());
}

#[tokio::test]
#[expect(
    clippy::significant_drop_tightening,
    reason = "the spawned serve future owns the server until the final join"
)]
async fn disabling_service_closes_active_websockets_and_rejects_new_upgrades() {
    let directory = tempfile::tempdir().unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let endpoint = Endpoint::parse(&format!("ws://{address}/v2/ws")).unwrap();
    let server = Server::open(
        endpoint,
        OriginPolicy::Public,
        SourcePolicy::default(),
        ConnectionLimits::default(),
        &directory.path().join("node.sqlite"),
    )
    .unwrap()
    .configure(Settings::default(), None)
    .unwrap();
    let node = server.node();
    let (stop, stopped) = tokio::sync::oneshot::channel();
    let task = tokio::spawn(async move {
        server
            .serve(listener, async {
                let _ = stopped.await;
            })
            .await
    });
    let url = format!("ws://{address}/v2/ws?share_id=AQEBAQEBAQEBAQEB");
    let (mut client, _) = tokio_tungstenite::connect_async(&url).await.unwrap();
    // A ping/pong proves the upgraded transport has entered its owned runtime.
    client
        .send(tokio_tungstenite::tungstenite::Message::Ping(
            vec![1].into(),
        ))
        .await
        .unwrap();
    let _ = tokio::time::timeout(Duration::from_secs(2), client.next())
        .await
        .unwrap();
    node.update(
        Settings {
            services: Services {
                relay: false,
                stun: true,
            },
            relay_budget: None,
        },
        now(),
    )
    .unwrap();
    assert!(node.services().stun);
    let ended = tokio::time::timeout(Duration::from_secs(2), client.next())
        .await
        .unwrap();
    assert!(ended.is_none_or(|message| message.is_err()
        || matches!(
            message,
            Ok(tokio_tungstenite::tungstenite::Message::Close(_))
        )));
    let error = tokio_tungstenite::connect_async(url).await.unwrap_err();
    assert!(
        matches!(error, tokio_tungstenite::tungstenite::Error::Http(response) if response.status() == 503)
    );
    let mut http = TcpStream::connect(address).await.unwrap();
    http.write_all(b"GET /.well-known/windshare HTTP/1.1\r\nHost: local\r\n\r\n")
        .await
        .unwrap();
    let mut response = String::new();
    http.read_to_string(&mut response).await.unwrap();
    let body: serde_json::Value =
        serde_json::from_str(response.split("\r\n\r\n").nth(1).unwrap()).unwrap();
    assert_eq!(body["services"]["relay"], false);
    assert_eq!(body["services"]["stun"], true);
    stop.send(()).unwrap();
    task.await.unwrap().unwrap();
}
