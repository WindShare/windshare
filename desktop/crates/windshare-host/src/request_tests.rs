use super::{Command, SetupInput};
use crate::{AccountCommand, PreferenceEdit, RelayOperation};

#[test]
fn preference_commands_log_the_action_and_keep_the_address_on_the_wire() {
    let address = "https://relay.example/private?route=query-fixture";
    let cases = [
        (PreferenceEdit::Enable(address.into()), "enable"),
        (PreferenceEdit::Disable(address.into()), "disable"),
        (PreferenceEdit::Remove(address.into()), "remove"),
        (PreferenceEdit::Frontend(address.into()), "frontend"),
    ];
    for (edit, action) in cases {
        let command = Command::Configure {
            operation_id: "share-operation".into(),
            setup: SetupInput {
                id: "preference-setup".into(),
                preference: Some(edit),
                ..SetupInput::default()
            },
        };
        let fields = command.log_fields();
        assert_eq!(fields.command, "configure");
        assert_eq!(fields.action, Some(action));
        assert_eq!(fields.operation_id, Some("share-operation"));
        assert_eq!(fields.setup_id, Some("preference-setup"));
        assert!(!format!("{fields:?}").contains("query-fixture"));
        let wire = serde_json::to_value(&command).unwrap();
        assert_eq!(
            wire["setup"]["preference"],
            serde_json::json!({ "action": action, "value": address })
        );
    }
}

#[test]
fn credential_commands_project_only_diagnostic_fields() {
    let account = |account| Command::Account {
        operation_id: "account-operation".into(),
        account,
    };
    let cases = [
        (
            account(AccountCommand::Login {
                username: "user".into(),
                password: "login-secret-fixture".into(),
            }),
            "login",
            "/account/password",
            "login-secret-fixture",
        ),
        (
            account(AccountCommand::Password {
                account_id: "target-account".into(),
                username: "user".into(),
                password: "new-password-fixture".into(),
            }),
            "password",
            "/account/password",
            "new-password-fixture",
        ),
        (
            account(AccountCommand::Verify {
                code: "email-code-fixture".into(),
            }),
            "verify",
            "/account/code",
            "email-code-fixture",
        ),
        (
            account(AccountCommand::Revoke {
                account_id: "target-account".into(),
                credential: "credential-id-fixture".into(),
            }),
            "revoke",
            "/account/credential",
            "credential-id-fixture",
        ),
    ];
    for (command, action, pointer, secret) in cases {
        let fields = command.log_fields();
        assert_eq!(fields.command, "account");
        assert_eq!(fields.action, Some(action));
        assert_eq!(fields.operation_id, Some("account-operation"));
        assert!(!format!("{fields:?}").contains(secret));
        let request = serde_json::to_value(&command).unwrap();
        assert_eq!(request.pointer(pointer).unwrap(), secret);
        if matches!(action, "password" | "revoke") {
            assert_eq!(request["account"]["account_id"], "target-account");
        }
    }

    let command = Command::Configure {
        operation_id: String::new(),
        setup: SetupInput {
            id: "relay-setup".into(),
            relay_url: "https://relay.example".into(),
            relay_operation: Some(RelayOperation::AddProfile),
            access_key: "relay-key-fixture".into(),
            ..SetupInput::default()
        },
    };
    let fields = command.log_fields();
    assert_eq!(fields.command, "configure");
    assert_eq!(fields.setup_id, Some("relay-setup"));
    assert!(!format!("{fields:?}").contains("relay-key-fixture"));
    let wire = serde_json::to_value(&command).unwrap();
    assert_eq!(wire["setup"]["relay_operation"], "add");
    assert_eq!(wire["setup"]["access_key"], "relay-key-fixture");
    assert_eq!(
        serde_json::to_value(RelayOperation::AuthorizeDevice).unwrap(),
        "authorize_device"
    );
}
