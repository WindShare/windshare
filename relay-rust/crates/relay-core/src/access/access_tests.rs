use super::*;
use crate::test_vectors::{bytes, session_case};
use core::num::NonZeroU64;

fn request() -> RegistrationRequest {
    let fixture: serde_json::Value = session_case("relay-capacity-registration");
    RegistrationRequest::from_frame(
        ConnectionRef {
            id: NonZeroU64::new(1).unwrap(),
            generation: NonZeroU64::new(2).unwrap(),
        },
        &bytes(fixture["registerInitB64"].as_str().unwrap()),
    )
    .unwrap()
}

#[test]
fn authorization_binds_request_attribution_and_shared_expiry_boundary() {
    let now = UnixMillis(100);
    let expected = request();
    let connection = expected.connection();
    let init = expected.init;
    let attribution = Attribution::Credential {
        principal: [1; 16],
        credential: [2; 16],
    };
    let authorized = expected.resolve(RegistrationDecision::Granted(attribution), now);
    assert_eq!(authorized.connection(), connection);
    assert_eq!(
        authorized.consume(UnixMillis(now.0 + REGISTRATION_GRANT_LIFETIME_MS - 1)),
        Ok((init, attribution)),
    );
    let expired = request().resolve(RegistrationDecision::Granted(attribution), now);
    assert_eq!(
        expired.consume(UnixMillis(now.0 + REGISTRATION_GRANT_LIFETIME_MS)),
        Err(ErrorFrame::AccessDenied),
    );
}

#[test]
fn policy_refusals_keep_connection_identity_and_wire_retry_semantics() {
    let now = UnixMillis(100);
    for (decision, refusal) in [
        (RegistrationDecision::Denied, ErrorFrame::AccessDenied),
        (
            RegistrationDecision::Unavailable,
            ErrorFrame::AccessUnavailable(RetryAfter::from_millis(ACCESS_RETRY_MS).unwrap()),
        ),
    ] {
        let request = request();
        let connection = request.connection();
        let authorization = request.resolve(decision, now);
        assert_eq!(authorization.connection(), connection);
        assert_eq!(authorization.consume(now), Err(refusal));
    }
}
