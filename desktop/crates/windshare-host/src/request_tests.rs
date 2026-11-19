use super::{Command, SetupInput};
use crate::{AccountCommand, RelayOperation};

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
    ];
    for (command, action, pointer, secret) in cases {
        let fields = command.log_fields();
        assert_eq!(fields.command, "account");
        assert_eq!(fields.action, Some(action));
        assert_eq!(fields.operation_id, Some("account-operation"));
        assert!(!format!("{fields:?}").contains(secret));
        assert_eq!(
            serde_json::to_value(&command)
                .unwrap()
                .pointer(pointer)
                .unwrap(),
            secret
        );
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
        "private-onboarding"
    );
}
