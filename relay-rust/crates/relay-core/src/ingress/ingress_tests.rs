extern crate std;

use super::*;
use crate::{
    flow::Counts,
    identifiers::RelaySessionId,
    registration::RegisterInit,
    test_vectors::{bytes, session_case},
};
use core::num::NonZeroU64;

fn connection() -> ConnectionRef {
    ConnectionRef {
        id: NonZeroU64::new(1).unwrap(),
        generation: NonZeroU64::new(2).unwrap(),
    }
}

#[test]
fn canonical_client_controls_share_one_inbound_vocabulary() {
    let fixture: serde_json::Value = session_case("relay-capacity-registration");
    for (field, kind) in [
        ("registerInitB64", FrameKind::RegisterInit),
        ("registerProofB64", FrameKind::RegisterProof),
        ("resumeCredentialB64", FrameKind::ResumeCredential),
        ("descriptorUploadB64", FrameKind::DescriptorUpload),
        ("stopInitB64", FrameKind::StopInit),
        ("stopProofB64", FrameKind::StopProof),
        ("joinB64", FrameKind::Join),
        ("sessionAdmittedB64", FrameKind::SessionAdmitted),
        ("connectionProbeB64", FrameKind::ConnectionProbe),
    ] {
        let frame = bytes(fixture[field].as_str().unwrap());
        assert_eq!(FrameKind::classify(&frame), kind, "{field}");
        assert!(!matches!(
            Control::inspect(connection(), &frame),
            Control::Ready(Input::Unsupported(_))
        ));
    }
    let session = RelaySessionId::new([1; 8]).unwrap();
    let consumed = DeliveryConsumed::new(session, Counts::new(1, 21))
        .unwrap()
        .encode();
    let settled = SettlementAck::new(session, Counts::new(1, 21))
        .unwrap()
        .encode();
    assert_eq!(FrameKind::classify(&consumed), FrameKind::DeliveryConsumed);
    assert_eq!(FrameKind::classify(&settled), FrameKind::SettlementAck);
}

#[test]
fn only_valid_fresh_registration_requests_host_authorization() {
    let fixture: serde_json::Value = session_case("relay-capacity-registration");
    let fresh = bytes(fixture["registerInitB64"].as_str().unwrap());
    let Control::Authorize(request) = Control::inspect(connection(), &fresh) else {
        panic!("fresh registration needs service authorization");
    };
    assert_eq!(request.connection(), connection());
    assert_eq!(
        request.registration(),
        RegisterInit::decode(&fresh).unwrap().registration(),
    );
    for frame in [
        bytes(fixture["resumeInitB64"].as_str().unwrap()),
        fresh[..fresh.len() - 1].to_vec(),
        b"WS2R".to_vec(),
    ] {
        assert!(matches!(
            Control::inspect(connection(), &frame),
            Control::Ready(Input::Control { connection: target, frame: original })
                if target == connection() && original == frame
        ));
    }
}

#[test]
fn unknown_short_and_response_headers_never_enter_control_protocol() {
    for frame in [&b""[..], &b"WS2"[..], b"WS2Z", b"WS2K", b"WS2E", b"WS2A"] {
        assert_eq!(FrameKind::classify(frame), FrameKind::Unsupported);
        assert!(matches!(
            Control::inspect(connection(), frame),
            Control::Ready(Input::Unsupported(target)) if target == connection()
        ));
    }
    assert_eq!(FrameKind::classify(b"WS2O"), FrameKind::Opaque);
    assert!(matches!(
        Control::inspect(connection(), b"WS2O"),
        Control::Ready(Input::Unsupported(_))
    ));
}
