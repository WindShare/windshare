use super::*;
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpStream,
};

async fn socket_pair() -> (TcpStream, TcpStream) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let client = TcpStream::connect(listener.local_addr().unwrap())
        .await
        .unwrap();
    let (server, _) = listener.accept().await.unwrap();
    (server, client)
}

#[tokio::test]
async fn http_slots_end_at_upgrade_and_non_upgrades_never_create_share_actors() {
    let (_directory, registry, _fixture) = registry();
    let registry = Arc::new(registry);
    let capacity = Capacity::new(
        ConnectionLimits {
            business: 1,
            controls: 1,
            business_per_source: 1,
            controls_per_source: 1,
        },
        CONNECTION_ATTEMPTS,
        registry.environment.now(),
    )
    .unwrap();
    let ingress = Arc::new(Ingress::new(
        registry.endpoint.clone(),
        OriginPolicy::default(),
        SourcePolicy::default(),
        capacity,
        registry.environment.clone(),
    ));
    let http_slots = Arc::new(Semaphore::new(1));
    let (shutdown, stopping) = watch::channel(false);
    for (request, status) in [
        ("GET /healthz HTTP/1.1\r\nHost: relay.test\r\n\r\n", 200),
        (
            "GET /v2/ws?share_id=AQEBAQEBAQEBAQEB HTTP/1.1\r\nHost: relay.test\r\nOrigin: null\r\n\r\n",
            403,
        ),
    ] {
        let (server, mut client) = socket_pair().await;
        let task = tokio::spawn(socket::serve(
            server,
            Arc::clone(&ingress),
            Arc::clone(&registry),
            reference(1),
            stopping.clone(),
            Arc::clone(&http_slots).try_acquire_owned().unwrap(),
        ));
        client.write_all(request.as_bytes()).await.unwrap();
        let mut response = String::new();
        timeout(TEST_BUDGET, client.read_to_string(&mut response))
            .await
            .unwrap()
            .unwrap();
        task.await.unwrap().unwrap();
        assert!(
            response.starts_with(&format!("HTTP/1.1 {status} ")),
            "{response}"
        );
        assert_eq!(http_slots.available_permits(), 1);
        assert!(registry.actors.lock().unwrap().is_empty());
    }
    let (server, client) = socket_pair().await;
    let task = tokio::spawn(socket::serve(
        server,
        ingress,
        Arc::clone(&registry),
        reference(2),
        stopping,
        Arc::clone(&http_slots).try_acquire_owned().unwrap(),
    ));
    let (client, _) =
        tokio_tungstenite::client_async("ws://relay.test/v2/ws?share_id=AQEBAQEBAQEBAQEB", client)
            .await
            .unwrap();
    let permit = timeout(TEST_BUDGET, http_slots.acquire())
        .await
        .unwrap()
        .unwrap();
    assert!(!task.is_finished());
    assert_eq!(registry.actors.lock().unwrap().len(), 1);
    shutdown.send_replace(true);
    timeout(TEST_BUDGET, task).await.unwrap().unwrap().unwrap();
    registry.shutdown().await.unwrap();
    drop(registry);
    drop(permit);
    drop(client);
}
