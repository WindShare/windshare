use super::*;

#[test]
fn origin_policy_is_validated_with_startup_options() {
    for args in [
        vec!["--database", "relay.sqlite"],
        vec!["--allow-localhost", "--database", "relay.sqlite"],
        vec![
            "--database",
            "relay.sqlite",
            "--origins",
            "https://app.example, https://other.example:8443",
        ],
    ] {
        assert!(options(args.into_iter().map(str::to_owned)).is_ok());
    }
    for args in [
        vec![
            "--database",
            "relay.sqlite",
            "--origins",
            "https://app.example/",
        ],
        vec![
            "--database",
            "relay.sqlite",
            "--origins",
            "https://app.example,",
        ],
        vec!["--database", "relay.sqlite", "--origins", ""],
        vec!["--database", "relay.sqlite", "--origins"],
        vec!["--allow-localhost"],
    ] {
        assert!(options(args.into_iter().map(str::to_owned)).is_err());
    }
}

#[test]
fn stun_options_validate_positive_limits_and_explicit_listener_families() {
    let parsed = options(
        [
            "--database",
            "relay.sqlite",
            "--stun-udp",
            "127.0.0.1:0, [::1]:0",
            "--stun-admin",
            "127.0.0.1:0",
            "--stun-requests-per-second",
            "100",
            "--stun-source-requests-per-second",
            "5",
            "--stun-max-sources",
            "32",
        ]
        .into_iter()
        .map(str::to_owned),
    )
    .unwrap();
    assert_eq!(parsed.stun.udp_addresses.len(), 2);
    assert_eq!(parsed.stun.limits.requests_per_second.get(), 100);
    assert_eq!(parsed.stun.limits.source_requests_per_second.get(), 5);
    assert_eq!(parsed.stun.limits.max_sources.get(), 32);
    let disabled = options(
        [
            "--database",
            "relay.sqlite",
            "--stun-udp",
            "",
            "--stun-admin",
            "",
        ]
        .into_iter()
        .map(str::to_owned),
    )
    .unwrap();
    assert_eq!(disabled.stun.udp_addresses, Vec::<SocketAddr>::new());
    assert!(disabled.stun.admin_address.is_none());
    for (flag, value) in [
        ("--stun-requests-per-second", "0"),
        ("--stun-source-requests-per-second", "-1"),
        ("--stun-max-sources", "0"),
        ("--stun-max-sources", "bad"),
        ("--stun-udp", "localhost:3478"),
        ("--stun-udp", "127.0.0.1:0,"),
        ("--stun-udp", "127.0.0.1:3478,127.0.0.1:3478"),
        ("--stun-admin", "bad"),
    ] {
        assert!(
            options(
                ["--database", "relay.sqlite", flag, value]
                    .into_iter()
                    .map(str::to_owned)
            )
            .is_err()
        );
    }
}

#[tokio::test]
async fn process_composition_shuts_down_and_releases_storage_after_startup_failure() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("relay.sqlite");
    let startup = || Options {
        listen: "127.0.0.1:0".parse().unwrap(),
        database: database.clone(),
        public_endpoint: Some("ws://127.0.0.1:8787/v2/ws".to_owned()),
        origins: OriginPolicy::default(),
        stun: stun::Config {
            udp_addresses: vec!["127.0.0.1:0".parse().unwrap()],
            admin_address: Some("127.0.0.1:0".parse().unwrap()),
            ..stun::Config::default()
        },
    };
    let mut broken = startup();
    broken.database = directory.path().join("missing").join("relay.sqlite");
    assert!(run(broken, async {}).await.is_err());
    for _ in 0..2 {
        tokio::time::timeout(
            std::time::Duration::from_secs(3),
            run(startup(), tokio::task::yield_now()),
        )
        .await
        .unwrap()
        .unwrap();
    }
}
