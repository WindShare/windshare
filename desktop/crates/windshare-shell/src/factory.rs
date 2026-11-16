// windows::implement generates these patterns in COM vtables.
#![allow(clippy::ref_as_ptr, clippy::inline_always)]

use std::{
    ffi::c_void,
    sync::atomic::{AtomicUsize, Ordering},
};
use windows::{
    Win32::{
        Foundation::{CLASS_E_CLASSNOTAVAILABLE, CLASS_E_NOAGGREGATION, E_POINTER, S_FALSE, S_OK},
        System::Com::{IClassFactory, IClassFactory_Impl},
        UI::Shell::IExplorerCommand,
    },
    core::{BOOL, GUID, HRESULT, IUnknown, Interface, Ref, implement},
};

static LIVE_OBJECTS: AtomicUsize = AtomicUsize::new(0);
pub struct Lifetime;
impl Lifetime {
    pub fn new() -> Self {
        LIVE_OBJECTS.fetch_add(1, Ordering::Relaxed);
        Self
    }
}
impl Drop for Lifetime {
    fn drop(&mut self) {
        LIVE_OBJECTS.fetch_sub(1, Ordering::Release);
    }
}

#[implement(IClassFactory)]
struct Factory {
    _lifetime: Lifetime,
}
impl IClassFactory_Impl for Factory_Impl {
    fn CreateInstance(
        &self,
        outer: Ref<IUnknown>,
        iid: *const GUID,
        object: *mut *mut c_void,
    ) -> windows::core::Result<()> {
        if outer.is_some() {
            return Err(CLASS_E_NOAGGREGATION.into());
        }
        if iid.is_null() || object.is_null() {
            return Err(E_POINTER.into());
        }
        let command: IExplorerCommand = super::command::ShareCommand::new().into();
        // SAFETY: COM supplies valid IID/output pointers, checked for null above.
        unsafe { command.query(iid, object).ok() }
    }
    fn LockServer(&self, lock: BOOL) -> windows::core::Result<()> {
        if lock.as_bool() {
            LIVE_OBJECTS.fetch_add(1, Ordering::Relaxed);
        } else {
            LIVE_OBJECTS.fetch_sub(1, Ordering::Release);
        }
        Ok(())
    }
}

/// # Safety
/// Called by COM with valid class/IID pointers and a writable interface output.
#[unsafe(no_mangle)]
pub unsafe extern "system" fn DllGetClassObject(
    class: *const GUID,
    iid: *const GUID,
    object: *mut *mut c_void,
) -> HRESULT {
    if class.is_null() || iid.is_null() || object.is_null() {
        return E_POINTER;
    }
    // SAFETY: COM's entry contract supplies valid pointers, null-checked above.
    unsafe {
        *object = std::ptr::null_mut();
        if *class != GUID::from_u128(crate::command_clsid()) {
            return CLASS_E_CLASSNOTAVAILABLE;
        }
        let factory: IClassFactory = Factory {
            _lifetime: Lifetime::new(),
        }
        .into();
        factory.query(iid, object)
    }
}

#[unsafe(no_mangle)]
pub extern "system" fn DllCanUnloadNow() -> HRESULT {
    if LIVE_OBJECTS.load(Ordering::Acquire) == 0 {
        S_OK
    } else {
        S_FALSE
    }
}
