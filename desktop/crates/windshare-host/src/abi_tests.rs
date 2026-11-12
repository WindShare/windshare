use super::Host;

#[test]
fn native_library_starts_authenticates_and_joins_twice() {
    for _ in 0..2 {
        crate::check_host().expect("packaged native host roundtrip");
    }
    let host = Host::start().unwrap();
    assert!(Host::start().is_err(), "one host per application");
    host.close().unwrap();
}
