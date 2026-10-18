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
