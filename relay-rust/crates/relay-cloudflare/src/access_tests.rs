use super::*;
use crate::runtime::RegistrationAccess;
use windshare_relay_core::error_frame::{ErrorFrame, RetryAfter};

#[test]
fn access_failure_is_retryable_without_claiming_sender_ownership() {
    let f = Fixture::new();
    let mut host = Memory::new(&f);
    let mut runtime = host.restore(1).unwrap();
    host.open(&mut runtime, 1);
    runtime
        .control(
            &mut host,
            connection(1),
            &f.frame("registerInitB64"),
            RegistrationAccess::Unavailable,
        )
        .unwrap();
    assert!(host.stored.is_none());
    assert!(host.sent.iter().any(|(_, bytes)| bytes
        == &ErrorFrame::AccessUnavailable(RetryAfter::from_millis(1_000).unwrap()).encode()));
}
