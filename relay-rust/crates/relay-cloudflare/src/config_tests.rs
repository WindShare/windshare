use super::*;
use crate::persistence::{Image, parse_tag, tag};
use windshare_relay_core::lifecycle::Incarnation;

#[test]
fn routing_and_durable_identity_are_bound_without_credentials() {
    let hint = URL_SAFE_NO_PAD.encode([7; 12]);
    let route = Route::parse(&format!("https://relay.example/v2/ws?share_id={hint}")).unwrap();
    assert_eq!(route.endpoint, "wss://relay.example/v2/ws");
    assert_eq!(route.object_name(), hint);
    let mut image = Image::new(&route);
    let first = image.allocate().unwrap();
    let mut restored: Image = serde_json::from_slice(&serde_json::to_vec(&image).unwrap()).unwrap();
    let second = restored.allocate().unwrap();
    assert_ne!(first, second);
    assert_eq!(parse_tag(&tag(second)), Some(second));
    assert_eq!(
        restored
            .identity(Incarnation::new([1; 16]).unwrap())
            .unwrap()
            .share,
        route.share
    );
    let other = Route::parse(&format!("https://other.example/v2/ws?share_id={hint}")).unwrap();
    assert!(restored.check_route(&other).is_err());
    for raw in [
        "https://relay.example/v2/ws".to_owned(),
        format!("https://relay.example/v2/ws?share_id={hint}&share_id={hint}"),
        format!("https://relay.example/v2/ws?share_id={hint}&token=secret"),
        "https://relay.example/v2/ws?share_id=AAAAAAAAAAAAAAAA".to_owned(),
        "https://relay.example/v2/ws?share_id=short".to_owned(),
        format!("https://relay.example/wrong?share_id={hint}"),
    ] {
        assert!(Route::parse(&raw).is_err(), "{raw}");
    }
    for invalid in ["c-0-1", "c-1-0", "c-1-1-extra", "role-sender"] {
        assert!(parse_tag(invalid).is_none());
    }
}

#[test]
fn alarms_only_move_earlier_and_profile_fits_binary_attachments() {
    assert_eq!(earlier_alarm(None, Some(10)), Some(10));
    assert_eq!(earlier_alarm(Some(20), Some(10)), Some(10));
    assert_eq!(earlier_alarm(Some(10), Some(20)), None);
    assert_eq!(earlier_alarm(Some(10), Some(10)), None);
    assert_eq!(earlier_alarm(Some(10), None), None);
    budget()
        .check(limits().retained_flows_per_connection)
        .unwrap();
}
