use super::*;
use std::{
    cell::Cell,
    ffi::CString,
    panic::{catch_unwind, panic_any},
    ptr,
};

thread_local! {
    static RELEASED_BUFFERS: Cell<usize> = const { Cell::new(0) };
}

unsafe extern "C" fn free_fixture(pointer: *mut c_char) {
    // SAFETY: fixture transfers an unchanged CString::into_raw allocation into
    // Buffer, whose sole owner invokes this matching deallocator exactly once.
    drop(unsafe { CString::from_raw(pointer) });
    RELEASED_BUFFERS.set(RELEASED_BUFFERS.get() + 1);
}

fn fixture(bytes: &[u8]) -> Buffer {
    let pointer = CString::new(bytes).unwrap().into_raw();
    // SAFETY: this fresh CString is NUL-terminated, uniquely owned and immutable.
    // free_fixture has process lifetime and returns it to the same allocator.
    unsafe { Buffer::from_raw(pointer, free_fixture) }.unwrap()
}

#[test]
fn decoded_values_survive_the_native_buffer() {
    let released = RELEASED_BUFFERS.get();
    let result = super::super::response(
        fixture(br#"{"abi_version":1,"handle":9,"endpoint":{"session_id":"fixture","address":"127.0.0.1:1","token":"secret","log_directory":"logs"}}"#).bytes(),
    )
    .unwrap();
    assert_eq!(RELEASED_BUFFERS.get(), released + 1);
    assert_eq!(result.handle, 9);
    assert_eq!(result.endpoint.unwrap().session_id, "fixture");
}

#[test]
fn every_decode_failure_releases_its_native_buffer() {
    for bytes in [
        b"invalid JSON".as_slice(),
        br#"{"abi_version":2}"#,
        br#"{"abi_version":1,"error":"host failed"}"#,
    ] {
        let released = RELEASED_BUFFERS.get();
        assert!(super::super::response(fixture(bytes).bytes()).is_err());
        assert_eq!(RELEASED_BUFFERS.get(), released + 1);
    }
}

#[test]
fn unwinding_releases_the_native_buffer_once() {
    let released = RELEASED_BUFFERS.get();
    let result = catch_unwind(|| {
        let buffer = fixture(b"retained until unwind");
        assert_eq!(buffer.bytes(), b"retained until unwind");
        panic_any("fixture panic");
    });
    assert!(result.is_err());
    assert_eq!(RELEASED_BUFFERS.get(), released + 1);
}

#[test]
fn null_results_never_reach_the_deallocator() {
    let released = RELEASED_BUFFERS.get();
    // SAFETY: null is explicitly accepted and rejected without dereferencing;
    // free_fixture remains callable for the entire attempted owner lifetime.
    let result = unsafe { Buffer::from_raw(ptr::null_mut(), free_fixture) };
    assert!(result.is_err());
    assert_eq!(RELEASED_BUFFERS.get(), released);
}
