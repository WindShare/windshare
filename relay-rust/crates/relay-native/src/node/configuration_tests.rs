use super::*;
use serde_json::json;

fn read(value: &serde_json::Value) -> (tempfile::TempDir, anyhow::Result<Configuration>) {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("node.json");
    std::fs::write(&path, serde_json::to_vec(value).unwrap()).unwrap();
    let result = Configuration::read(&path);
    (directory, result)
}
fn fixture() -> serde_json::Value {
    json!({"listen":"127.0.0.1:0","database":"relay.sqlite","identity_file":"identity.json",
        "stun":{"udp_addresses":[],"admin_address":null},
        "node":{"services":{"relay":true,"stun":false}}})
}
#[test]
fn zone_defaults_paths_and_explicit_policy_have_one_configuration_owner() {
    let mut value = fixture();
    value["node"]["zone"] = "cn".into();
    let (directory, config) = read(&value);
    let config = config.unwrap();
    assert_eq!(config.database, directory.path().join("relay.sqlite"));
    assert_eq!(config.identity_file, directory.path().join("identity.json"));
    assert!(!config.node.access.anonymous);
    value["node"]["access"] = json!({"anonymous":true});
    assert!(read(&value).1.unwrap().node.access.anonymous);
    value["node"]["zone"] = "global".into();
    value["node"].as_object_mut().unwrap().remove("access");
    assert!(read(&value).1.unwrap().node.access.anonymous);
}
#[test]
fn invalid_ingress_configuration_is_rejected_before_startup() {
    let cases = [
        ("listen", json!("0.0.0.0:8484")),
        ("proxy", json!({"peers":[],"header":"CF-Connecting-IP"})),
        ("connections", json!({"business":0})),
        (
            "origins",
            json!({"kind":"allowlist","origins":["https://front.example/path"]}),
        ),
        ("obsolete_flag", json!(true)),
    ];
    for (field, invalid) in cases {
        let mut value = fixture();
        value[field] = invalid;
        assert!(read(&value).1.is_err(), "{field} unexpectedly accepted");
    }
}
#[test]
fn shipped_self_hosted_example_is_valid() {
    let value: serde_json::Value = serde_json::from_str(include_str!(
        "../../../../../deploy/wsrelay/node.example.json"
    ))
    .unwrap();
    assert!(read(&value).1.is_ok());
}

#[test]
fn invalid_relay_budget_reports_field_specific_validation_errors() {
    let mut bad_day = fixture();
    bad_day["node"]["relay_budget"] = json!({"bytes": 1024, "reset_day_utc": 29});
    assert!(
        read(&bad_day)
            .1
            .err()
            .expect("reset day outside 1..=28 must fail")
            .to_string()
            .contains("billing reset day must be 1..28 UTC")
    );

    let mut oversized = fixture();
    oversized["node"]["relay_budget"] = json!({"bytes": u64::MAX, "reset_day_utc": 1});
    assert!(
        read(&oversized)
            .1
            .err()
            .expect("budget above i64::MAX must fail")
            .to_string()
            .contains("relay budget exceeds storage range")
    );
}
