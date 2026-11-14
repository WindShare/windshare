use anyhow::{Context as _, Result, bail};
use libloading::Library;
use std::{
    ffi::{CStr, c_char},
    mem::ManuallyDrop,
    path::PathBuf,
    ptr::NonNull,
    sync::OnceLock,
};

#[cfg(test)]
#[path = "native_tests.rs"]
mod tests;

type Start = unsafe extern "C" fn() -> *mut c_char;
type Stop = unsafe extern "C" fn(u64) -> *mut c_char;
type Free = unsafe extern "C" fn(*mut c_char);

pub(super) struct Api {
    start: Start,
    stop: Stop,
    free: Free,
    _library: ManuallyDrop<Library>,
}

// Go's runtime cannot be safely unloaded, including failed symbol resolution
// and concurrent initialization. Every successfully loaded library stays resident.
static API: OnceLock<Api> = OnceLock::new();

impl Api {
    pub(super) fn get() -> Result<&'static Self> {
        if let Some(api) = API.get() {
            return Ok(api);
        }
        let api = Self::load()?;
        if API.set(api).is_err() {
            bail!("host ABI initialized concurrently");
        }
        API.get().context("host ABI initialization")
    }

    fn load() -> Result<Self> {
        let path = library_path()?;
        tracing::debug!(event = "desktop.host_library_loading", path = %path.display());
        // SAFETY: build.rs builds cmd/windshare-host for this native target, and
        // packaging supplies that library beside the executable. Its Go runtime
        // initialization requires no caller-owned data. ManuallyDrop prevents
        // unloading its code, even if resolving an export below fails.
        let library = ManuallyDrop::new(
            unsafe { Library::new(&path) }
                .with_context(|| format!("load native host library {}", path.display()))?,
        );
        // SAFETY: these signatures match cmd/windshare-host/main.go's C exports:
        // no arguments / a 64-bit unsigned handle -> C char*, and C char* -> void.
        // All symbols come from the same resident library, pairing results with
        // their allocator's Free function for their entire lifetime.
        unsafe {
            Ok(Self {
                start: *library.get(b"WindShareHostStart\0")?,
                stop: *library.get(b"WindShareHostStop\0")?,
                free: *library.get(b"WindShareHostFree\0")?,
                _library: library,
            })
        }
    }

    pub(super) fn start(&self) -> Result<Buffer> {
        // SAFETY: the resident Start export takes no arguments; Go serializes
        // host ownership and returns a fresh C.CString for success and failure.
        // That allocation is transferred directly to its matching Free owner.
        unsafe { Buffer::from_raw((self.start)(), self.free) }
    }

    pub(super) fn stop(&self, handle: u64) -> Result<Buffer> {
        // SAFETY: Go validates this integer in its registry before accessing the
        // host; invalid handles also return a fresh C.CString. The resident
        // exports pair that allocation with its matching Free function.
        unsafe { Buffer::from_raw((self.stop)(handle), self.free) }
    }
}

pub(super) struct Buffer {
    pointer: NonNull<c_char>,
    free: Free,
}

impl Buffer {
    /// # Safety
    /// A non-null pointer must transfer exclusive ownership of a live, immutable,
    /// NUL-terminated allocation, readable through its terminator within one
    /// allocation of at most `isize::MAX` bytes. It must be releasable exactly once
    /// by `free`. The function and its library must remain valid until this buffer
    /// is dropped, and freeing must not unwind across the C ABI.
    unsafe fn from_raw(pointer: *mut c_char, free: Free) -> Result<Self> {
        Ok(Self {
            pointer: NonNull::new(pointer).context("host returned a null ABI result")?,
            free,
        })
    }

    pub(super) const fn bytes(&self) -> &[u8] {
        // SAFETY: from_raw establishes a live NUL-terminated allocation that
        // cannot be mutated or freed elsewhere. The borrow cannot outlive self.
        unsafe { CStr::from_ptr(self.pointer.as_ptr()).to_bytes() }
    }
}

impl Drop for Buffer {
    fn drop(&mut self) {
        // SAFETY: this non-cloneable owner still holds the original allocation,
        // and from_raw guarantees its matching deallocator remains callable.
        unsafe { (self.free)(self.pointer.as_ptr()) };
    }
}

fn library_path() -> Result<PathBuf> {
    // Tests use their own build output; relocated applications only trust the
    // packaged sibling, independently of the working directory or search path.
    let directory = if cfg!(test) {
        PathBuf::from(env!("OUT_DIR"))
    } else {
        std::env::current_exe()?
            .parent()
            .context("executable directory")?
            .to_path_buf()
    };
    let name = if cfg!(target_os = "windows") {
        "windshare_host.dll"
    } else if cfg!(target_os = "macos") {
        "libwindshare_host.dylib"
    } else {
        "libwindshare_host.so"
    };
    Ok(directory.join(name))
}
