use super::*;

#[test]
fn upgrade_hint_is_single_canonical_share_and_never_authority() {
    let share = ShareId::new([1; 12]).unwrap();
    let value = URL_SAFE_NO_PAD.encode(share.as_bytes());
    let request = |uri: String| Request::builder().uri(uri).body(()).unwrap();
    assert_eq!(
        share_hint(&request(format!("/v2/ws?share_id={value}"))).unwrap(),
        share
    );
    for uri in [
        "/v2/ws".to_owned(),
        format!("/v2/ws?share_id={value}&share_id={value}"),
        "/v2/ws?share_id=short".to_owned(),
        "/v2/ws?share_id=AAAAAAAAAAAAAAAA".to_owned(),
        format!("/v2/ws?share_id={value}&secret=x"),
    ] {
        assert!(share_hint(&request(uri)).is_err());
    }
}
