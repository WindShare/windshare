use std::os::windows::io::{AsRawHandle, FromRawHandle, OwnedHandle};
use windows::Win32::Foundation::{HANDLE, HLOCAL, LocalFree};
use windows::core::PWSTR;

pub struct Apartment(std::marker::PhantomData<std::rc::Rc<()>>);
impl Apartment {
    pub fn new(kind: windows::Win32::System::WinRT::RO_INIT_TYPE) -> windows::core::Result<Self> {
        // SAFETY: each caller creates this thread-bound guard before other COM objects.
        unsafe {
            windows::Win32::System::WinRT::RoInitialize(kind)?;
        }
        Ok(Self(std::marker::PhantomData))
    }
}
impl Drop for Apartment {
    fn drop(&mut self) {
        // SAFETY: the thread-bound guard balances its successful RoInitialize call.
        unsafe {
            windows::Win32::System::WinRT::RoUninitialize();
        }
    }
}

pub struct Handle(OwnedHandle);

impl Handle {
    pub fn new(handle: HANDLE) -> windows::core::Result<Self> {
        if handle.is_invalid() {
            return Err(windows::core::Error::from_thread());
        }
        // SAFETY: successful Win32 creation transfers one owned, non-null handle.
        Ok(Self(unsafe { OwnedHandle::from_raw_handle(handle.0) }))
    }
    pub fn raw(&self) -> HANDLE {
        HANDLE(self.0.as_raw_handle())
    }
}

pub struct LocalAllocation(pub *mut std::ffi::c_void);
impl Drop for LocalAllocation {
    fn drop(&mut self) {
        // SAFETY: this allocation comes exclusively from LocalAlloc-family APIs.
        unsafe {
            LocalFree(Some(HLOCAL(self.0)));
        }
    }
}

pub fn take_local_string(value: PWSTR) -> windows::core::Result<String> {
    let _allocation = LocalAllocation(value.0.cast());
    // SAFETY: the Windows API returned a terminated string owned by this scope.
    unsafe { Ok(value.to_string()?) }
}

pub fn wide(value: impl AsRef<std::ffi::OsStr>) -> Vec<u16> {
    use std::os::windows::ffi::OsStrExt;
    value.as_ref().encode_wide().chain(Some(0)).collect()
}
