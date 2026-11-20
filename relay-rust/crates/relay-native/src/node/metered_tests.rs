use super::*;
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

impl WriteOrigin {
    pub(super) fn test_public() -> Self {
        Self {
            share: windshare_relay_core::identifiers::ShareId::new([3; 12]).unwrap(),
            bearer: None,
            usage: usage::Meter::default(),
        }
    }
}

#[tokio::test]
async fn pending_unlimited_write_cannot_spend_credit_after_budget_is_enabled() {
    let directory = tempfile::tempdir().unwrap();
    let endpoint = crate::Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let store = Database::open(&directory.path().join("node.sqlite"), endpoint.identity).unwrap();
    let node = Runtime::new(Settings::default(), store).unwrap();
    let (stream, mut reader) = tokio::io::duplex(1);
    let mut writer = Metered::new(
        stream,
        node.clone(),
        node.epoch(),
        WriteOrigin::test_public(),
    );
    writer.write_all(b"a").await.unwrap();
    assert!(
        tokio::time::timeout(Duration::from_millis(10), writer.write(b"unmetered-credit"))
            .await
            .is_err()
    );
    node.update(
        Settings {
            authority: None,
            zone: "global".into(),
            region: String::new(),
            stun_urls: Vec::new(),
            access: super::access::Policy::default(),
            contribution: ContributionLimits::default(),
            capacity_bytes_per_second: None,
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

fn public_and_owner_node(
    monthly: u64,
    daily: u64,
) -> (tempfile::TempDir, Runtime, WriteOrigin, WriteOrigin) {
    use windshare_relay_access::{Credential, CredentialState, Digest};
    let directory = tempfile::tempdir().unwrap();
    let endpoint = crate::Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let store = Database::open(&directory.path().join("node.sqlite"), endpoint.identity).unwrap();
    let owner = Credential {
        principal: [5; 16],
        id: [6; 16],
        name: "owner".into(),
        digest: Digest::of(b"owner-token"),
        created_at: 0,
        last_used_at: None,
        state: CredentialState::Active,
    };
    let node = Runtime::new(
        Settings {
            access: access::Policy {
                owner_principals: vec![owner.principal],
                credentials: vec![owner],
                ..Default::default()
            },
            relay_budget: Some(MonthlyBudget {
                bytes: monthly,
                reset_day_utc: 1,
            }),
            contribution: ContributionLimits {
                daily_bytes: Some(daily),
                ..Default::default()
            },
            ..Settings::default()
        },
        store,
    )
    .unwrap();
    let owner_origin = WriteOrigin {
        share: windshare_relay_core::identifiers::ShareId::new([5; 12]).unwrap(),
        bearer: Some(access::Bearer::new("owner-token")),
        usage: usage::Meter::default(),
    };
    (directory, node, WriteOrigin::test_public(), owner_origin)
}

#[tokio::test]
async fn public_exhaustion_spares_owner_registration_and_its_uncredentialed_receiver_until_monthly_limit()
 {
    use windshare_relay_access::Attribution;
    let (_dir, node, public, owner) = public_and_owner_node(12, 4);
    let _guest = node
        .access
        .reserve(public.share, Attribution::Anonymous)
        .unwrap();
    let epoch = node.epoch();
    let write = |origin| Metered::new(tokio::io::sink(), node.clone(), epoch, origin);
    let mut guest = write(public.clone());
    let mut sender = write(owner.clone());
    assert_eq!(guest.write(b"public").await.unwrap(), 4);
    assert!(guest.write(b"more").await.is_err());
    assert!(node.available());
    assert!(node.public_budget_refuses(public.share, None));
    assert!(!node.public_budget_refuses(owner.share, owner.bearer.as_ref()));
    // An unresident share with an explicit non-owner bearer (such as an official ticket)
    // is refused at upgrade, while an unauthenticated join still defers to actor recovery.
    let unresident = windshare_relay_core::identifiers::ShareId::new([9; 12]).unwrap();
    let non_owner_bearer = access::Bearer::new("official-account-ticket");
    assert!(node.public_budget_refuses(unresident, Some(&non_owner_bearer)));
    assert!(!node.public_budget_refuses(unresident, None));
    sender.write_all(b"hi").await.unwrap();

    let attribution = Attribution::Credential {
        principal: [5; 16],
        credential: [6; 16],
    };
    let _residency = node.access.reserve(owner.share, attribution).unwrap();
    assert!(!node.public_budget_refuses(owner.share, None));
    let mut receiver = write(WriteOrigin {
        bearer: None,
        ..owner
    });
    receiver.write_all(b"123456").await.unwrap();
    assert!(receiver.write(b"over monthly").await.is_err());
    assert!(!node.available());
}

#[tokio::test]
async fn rejected_public_transaction_preserves_monthly_credit_for_owner() {
    let (dir, node, public, owner) = public_and_owner_node(16, 16);
    let db = rusqlite::Connection::open(dir.path().join("node.sqlite")).unwrap();
    db.execute_batch(
        "CREATE TRIGGER fail_public BEFORE INSERT ON public_relay_day
                      BEGIN SELECT RAISE(ABORT, 'rejected public credit'); END;",
    )
    .unwrap();
    let mut guest = Metered::new(tokio::io::sink(), node.clone(), node.epoch(), public);
    assert!(guest.write(b"x").await.is_err());
    let reserved: i64 = db
        .query_row("SELECT reserved FROM relay_budget", [], |row| row.get(0))
        .unwrap();
    assert_eq!(reserved, 0);
    let mut owner = Metered::new(tokio::io::sink(), node.clone(), node.epoch(), owner);
    owner.write_all(b"0123456789abcdef").await.unwrap();
}

#[tokio::test]
async fn pending_write_observes_removed_owner_status() {
    use futures_util::FutureExt;
    let (_dir, node, _public, owner) = public_and_owner_node(100, 0);
    let (stream, mut reader) = tokio::io::duplex(1);
    let mut writer = Metered::new(stream, node.clone(), node.epoch(), owner);
    writer.write_all(b"a").await.unwrap();
    assert!(writer.write(b"pending").now_or_never().is_none());
    let mut settings = node.state.lock().unwrap().settings.clone();
    settings.access.owner_principals.clear();
    node.update(settings, now()).unwrap();
    reader.read_u8().await.unwrap();
    assert!(writer.write(b"pending").await.is_err());
    assert!(node.available());
}

#[test]
fn pending_public_write_cannot_spend_yesterdays_allowance_at_midnight() {
    use budget::WriteAttempt;
    use std::task::Poll;
    let (_dir, node, public, _) = public_and_owner_node(100, 4);
    let today = now();
    let tomorrow = today + chrono::TimeDelta::days(1).num_seconds();
    node.refill_budgets(&public, 4, today).unwrap();
    let pending = node
        .write_egress(node.epoch(), &public, 4, today, |_| Poll::Pending)
        .unwrap();
    assert!(matches!(pending, WriteAttempt::Socket(Poll::Pending)));
    let write = |time| {
        node.write_egress(node.epoch(), &public, 4, time, |length| {
            Poll::Ready(Ok(length))
        })
    };
    assert!(matches!(write(tomorrow).unwrap(), WriteAttempt::Refill));
    node.refill_budgets(&public, 4, tomorrow).unwrap();
    assert!(matches!(
        write(tomorrow).unwrap(),
        WriteAttempt::Socket(Poll::Ready(Ok(4)))
    ));
    assert!(write(tomorrow).is_err());
}

#[tokio::test]
async fn stalled_public_socket_leaves_unused_monthly_credit_for_an_owner() {
    use futures_util::FutureExt;
    let (_dir, node, public, owner) = public_and_owner_node(8, 8);
    let (stream, _reader) = tokio::io::duplex(1);
    let mut guest = Metered::new(stream, node.clone(), node.epoch(), public);
    let mut owner = Metered::new(tokio::io::sink(), node.clone(), node.epoch(), owner);
    guest.write_all(b"x").await.unwrap();
    assert!(guest.write(b"1234567").now_or_never().is_none());
    owner.write_all(b"1234567").await.unwrap();
    assert!(owner.write(b"over monthly").await.is_err());
}
