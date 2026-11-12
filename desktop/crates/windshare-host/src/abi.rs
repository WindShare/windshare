use anyhow::{Context as _, Result, bail};
use libloading::Library;
use serde::Deserialize;
use std::{
    ffi::{CStr, c_char},
    mem::ManuallyDrop,
    path::PathBuf,
    sync::OnceLock,
};

#[cfg(test)]
#[path = "abi_tests.rs"]
mod tests;

type Start = unsafe extern "C" fn() -> *mut c_char;
type Stop = unsafe extern "C" fn(u64) -> *mut c_char;
type Free = unsafe extern "C" fn(*mut c_char);

#[derive(Clone, Deserialize)]
pub struct Endpoint {
    pub session_id: String,
    pub address: String,
    pub token: String,
    pub log_directory: String,
}

#[derive(Deserialize)]
struct Response {
    abi_version: u32,
    #[serde(default)]
    handle: u64,
    endpoint: Option<Endpoint>,
    #[serde(default)]
    error: String,
}

struct Api {
    start: Start,
    stop: Stop,
    free: Free,
    _library: ManuallyDrop<Library>,
}

// Go's runtime cannot be safely unloaded. The library remains process-resident;
// Stop releases the engine and its resources, not the Go runtime's code pages.
static API: OnceLock<Api> = OnceLock::new();

fn library_path() -> Result<PathBuf> {
    let executable = std::env::current_exe()?;
    let directory = executable.parent().context("executable directory")?;
    let directory = if directory.file_name().is_some_and(|name| name == "deps") {
        directory.parent().context("Cargo test directory")?
    } else {
        directory
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

fn api() -> Result<&'static Api> {
    if let Some(api) = API.get() {
        return Ok(api);
    }
    // The absolute path prevents the working directory or DLL search path from
    // selecting another library. Export signatures use only C scalars/buffers.
    let library = ManuallyDrop::new(unsafe { Library::new(library_path()?)? });
    let api = unsafe {
        Api {
            start: *library.get(b"WindShareHostStart\0")?,
            stop: *library.get(b"WindShareHostStop\0")?,
            free: *library.get(b"WindShareHostFree\0")?,
            _library: library,
        }
    };
    // Only the application worker starts the host. A second initializer would
    // be a caller ownership bug, not permission to unload the extra Go library.
    if API.set(api).is_err() {
        bail!("host ABI initialized concurrently");
    }
    API.get().context("host ABI initialization")
}

fn response(api: &Api, pointer: *mut c_char) -> Result<Response> {
    if pointer.is_null() {
        bail!("host returned a null ABI result");
    }
    // Copy before freeing, including malformed/error responses. Rust never
    // frees Go/C allocations with its own allocator or retains borrowed bytes.
    let bytes = unsafe { CStr::from_ptr(pointer).to_bytes().to_vec() };
    unsafe { (api.free)(pointer) };
    let result: Response = serde_json::from_slice(&bytes)?;
    if result.abi_version != 1 {
        bail!("unsupported host ABI version {}", result.abi_version);
    }
    if !result.error.is_empty() {
        bail!("{}", result.error);
    }
    Ok(result)
}

pub struct Host {
    handle: u64,
    api: &'static Api,
    pub endpoint: Endpoint,
}

impl Host {
    pub fn start() -> Result<Self> {
        let api = api()?;
        let result = response(api, unsafe { (api.start)() })?;
        Ok(Self {
            handle: result.handle,
            api,
            endpoint: result.endpoint.context("host endpoint missing")?,
        })
    }

    pub fn close(mut self) -> Result<()> {
        let handle = std::mem::take(&mut self.handle);
        response(self.api, unsafe { (self.api.stop)(handle) })?;
        Ok(())
    }
}

impl Drop for Host {
    fn drop(&mut self) {
        if self.handle != 0
            && let Err(error) = response(self.api, unsafe { (self.api.stop)(self.handle) })
        {
            tracing::error!(event = "desktop.host_drop_failed", %error);
        }
    }
}
