use super::*;
use serde_json::json;

#[test]
fn distinguishes_no_data_from_incomplete_or_nonpublic_answers() {
    let reply = |status, tc, answers| {
        serde_json::to_vec(&json!({"Status":status,"TC":tc,"Answer":answers})).unwrap()
    };
    assert_eq!(public_answers(&reply(0, false, json!([])), 28).unwrap(), 0);
    assert_eq!(
        public_answers(&reply(0, false, json!([{"type":1,"data":"1.1.1.1"}])), 1).unwrap(),
        1
    );
    for bytes in [
        reply(0, true, json!([])),
        reply(2, false, json!([])),
        reply(0, false, json!([{"type":1,"data":"2606:4700::1"}])),
        reply(0, false, json!([{"type":28,"data":"3fff:ffa::1"}])),
        reply(0, false, json!([{"type":1,"data":"192.88.99.1"}])),
    ] {
        assert!(public_answers(&bytes, 1).is_err());
    }
}
