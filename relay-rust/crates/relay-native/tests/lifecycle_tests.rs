use std::{
    path::{Path, PathBuf},
    process::{Command, Output},
};
use tokio::{net::TcpListener, sync::oneshot};
use windshare_relay_core::lifecycle::Record;
use windshare_relay_native::{ConnectionLimits, Endpoint, OriginPolicy, Server, SourcePolicy};

fn root() -> PathBuf {
    // Prefer Cargo's runtime CARGO_MANIFEST_DIR so cached test binaries reused
    // across worktrees locate the active repository root.
    std::env::var_os("CARGO_MANIFEST_DIR")
        .map_or_else(|| PathBuf::from(env!("CARGO_MANIFEST_DIR")), PathBuf::from)
        .join("../../..")
        .canonicalize()
        .unwrap()
}

fn client() -> (tempfile::TempDir, PathBuf) {
    let directory = tempfile::tempdir().unwrap();
    let binary = directory.path().join(if cfg!(windows) {
        "client.exe"
    } else {
        "client"
    });
    let output = Command::new("go")
        .current_dir(root())
        .env("GOWORK", "off")
        .args(["build", "-o"])
        .arg(&binary)
        .arg("./relay-rust/tests/_goclient")
        .output()
        .unwrap();
    success(&output);
    (directory, binary)
}

fn success(output: &Output) {
    assert!(
        output.status.success(),
        "stdout: {}\nstderr: {}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
}

fn stopped(database: &Path) {
    let connection = rusqlite::Connection::open(database).unwrap();
    let mut query = connection
        .prepare("SELECT record, descriptor IS NULL FROM shares")
        .unwrap();
    let records = query
        .query_map([], |row| {
            Ok((row.get::<_, Vec<u8>>(0)?, row.get::<_, bool>(1)?))
        })
        .unwrap();
    let records = records.collect::<rusqlite::Result<Vec<_>>>().unwrap();
    assert_eq!(records.len(), 1);
    assert!(matches!(
        Record::decode(&records[0].0).unwrap(),
        Record::Stopped(_)
    ));
    assert!(records[0].1);
}

const TEST_OWNER_TOKEN: &str = "native-interop-owner";

fn owner_node() -> windshare_relay_native::node::Settings {
    use windshare_relay_access::{Credential, Digest};
    use windshare_relay_native::node::{ContributionLimits, Identity, Settings};
    let owner = Credential {
        digest: Digest::of(TEST_OWNER_TOKEN.as_bytes()),
        ..Identity::generate().unwrap().owner()
    };
    let mut settings = Settings {
        // Only the sender presents its key. Charging JOINs or forwarded frames
        // to the receiver would exhaust this allowance during the exchange.
        contribution: ContributionLimits {
            daily_bytes: Some(8192),
            bytes_per_second: Some(1),
        },
        ..Settings::default()
    };
    settings.access.owner_principals.push(owner.principal);
    settings.access.credentials.push(owner);
    settings
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn go_runtime_client_drives_native_io_and_process_restart() {
    let (directory, client) = tokio::task::spawn_blocking(client).await.unwrap();
    let database = directory.path().join("in-process.sqlite");
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let endpoint = format!("ws://{}/v2/ws", listener.local_addr().unwrap());
    let server = Server::open(
        Endpoint::parse(&endpoint).unwrap(),
        OriginPolicy::default(),
        SourcePolicy::default(),
        ConnectionLimits::default(),
        &database,
    )
    .unwrap()
    .configure(owner_node(), None)
    .unwrap();
    let (shutdown, ended) = oneshot::channel();
    let task = tokio::spawn(async move {
        server
            .serve(listener, async {
                let _ = ended.await;
            })
            .await
    });
    let helper = client.clone();
    let result = tokio::task::spawn_blocking(move || {
        Command::new(helper)
            .env("WINDSHARE_RELAY_ACCESS_KEY", TEST_OWNER_TOKEN)
            .args(["-endpoint", &endpoint])
            .output()
            .unwrap()
    })
    .await
    .unwrap();
    shutdown.send(()).unwrap();
    task.await.unwrap().unwrap();
    success(&result);
    stopped(&database);

    let budget_database = directory.path().join("budget.sqlite");
    let mut exhausted = owner_node();
    exhausted.contribution.daily_bytes = Some(0);
    let settings = serde_json::to_string(&exhausted).unwrap();
    let database = directory.path().join("process.sqlite");
    let process_database = database.clone();
    let result = tokio::task::spawn_blocking(move || {
        let budget_case = Command::new(&client)
            .arg("-binary")
            .arg(env!("CARGO_BIN_EXE_relay-native"))
            .arg("-database")
            .arg(budget_database)
            .arg("-budget-node")
            .arg(settings)
            .env("WINDSHARE_RELAY_ACCESS_KEY", TEST_OWNER_TOKEN)
            .output()
            .unwrap();
        success(&budget_case);
        Command::new(client)
            .arg("-binary")
            .arg(env!("CARGO_BIN_EXE_relay-native"))
            .arg("-database")
            .arg(process_database)
            .output()
            .unwrap()
    })
    .await
    .unwrap();
    success(&result);
    stopped(&database);
}

#[tokio::test]
async fn shutdown_joins_connected_sockets_and_incomplete_upgrades() {
    use futures_util::StreamExt;
    use std::time::Duration;
    use tokio::{io::AsyncReadExt, net::TcpStream, time::timeout};

    const SHUTDOWN_BUDGET: Duration = Duration::from_secs(3);
    let directory = tempfile::tempdir().unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let endpoint = format!("ws://{address}/v2/ws");
    let server = Server::open(
        Endpoint::parse(&endpoint).unwrap(),
        OriginPolicy::default(),
        SourcePolicy::default(),
        ConnectionLimits::default(),
        &directory.path().join("shutdown.sqlite"),
    )
    .unwrap();
    let (shutdown, ended) = oneshot::channel();
    let task = tokio::spawn(async move {
        server
            .serve(listener, async {
                let _ = ended.await;
            })
            .await
    });
    let (mut socket, _) =
        tokio_tungstenite::connect_async(format!("{endpoint}?share_id=AQEBAQEBAQEBAQEB"))
            .await
            .unwrap();
    let mut upgrading = TcpStream::connect(address).await.unwrap();
    shutdown.send(()).unwrap();
    timeout(SHUTDOWN_BUDGET, task)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    assert!(
        timeout(SHUTDOWN_BUDGET, socket.next())
            .await
            .unwrap()
            .is_none_or(|value| value.is_err())
    );
    let mut byte = [0; 1];
    let received = timeout(SHUTDOWN_BUDGET, upgrading.read(&mut byte))
        .await
        .unwrap();
    assert!(received.is_err() || received.unwrap() == 0);
}
