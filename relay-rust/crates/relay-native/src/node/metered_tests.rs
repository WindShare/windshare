use super::*;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

#[tokio::test]
async fn pending_unlimited_write_cannot_spend_credit_after_budget_is_enabled() {
    let directory = tempfile::tempdir().unwrap();
    let endpoint = crate::Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let store = Database::open(&directory.path().join("node.sqlite"), endpoint.identity).unwrap();
    let node = Runtime::new(Settings::default(), store).unwrap();
    let (stream, mut reader) = tokio::io::duplex(1);
    let mut writer = Metered::new(stream, node.clone(), node.epoch());
    writer.write_all(b"a").await.unwrap();
    assert!(
        tokio::time::timeout(Duration::from_millis(10), writer.write(b"unmetered-credit"))
            .await
            .is_err()
    );
    node.update(
        Settings {
            services: Services::default(),
            relay_budget: Some(MonthlyBudget {
                bytes: 2,
                reset_day_utc: 1,
            }),
        },
        now(),
    )
    .unwrap();
    let mut byte = [0];
    reader.read_exact(&mut byte).await.unwrap();
    writer.write_all(b"b").await.unwrap();
    reader.read_exact(&mut byte).await.unwrap();
    writer.write_all(b"c").await.unwrap();
    reader.read_exact(&mut byte).await.unwrap();
    assert!(writer.write_all(b"over-budget").await.is_err());
    assert!(!node.available());
}
