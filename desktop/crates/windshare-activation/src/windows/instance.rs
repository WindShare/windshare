use super::{
    handles::{Handle, wide},
    security::{Security, logon_sid},
};
use std::{marker::PhantomData, rc::Rc};
use windows::{
    Win32::{
        Foundation::{ERROR_ALREADY_EXISTS, GetLastError, WAIT_ABANDONED, WAIT_OBJECT_0},
        System::Threading::{CreateMutexW, ReleaseMutex, WaitForSingleObject},
    },
    core::PCWSTR,
};

#[derive(Clone)]
pub struct Scope {
    pub sid: String,
    pub pipe: String,
    mutex: String,
}

impl Scope {
    /// # Errors
    /// Returns a failure to identify the current logon session.
    pub fn current() -> anyhow::Result<Self> {
        let channel = option_env!("WINDSHARE_INSTALL_CHANNEL").unwrap_or("development");
        Self::named(channel)
    }

    pub(super) fn named(channel: &str) -> anyhow::Result<Self> {
        let sid = logon_sid()?;
        let name = format!("WindShare.{channel}.{sid}");
        Ok(Self {
            pipe: format!(r"\\.\pipe\{name}.owner"),
            mutex: format!(r"Local\{name}"),
            sid,
        })
    }

    pub(super) fn dispatch_pipe(&self, endpoint: &str) -> anyhow::Result<String> {
        let endpoint = uuid::Uuid::parse_str(endpoint)?;
        Ok(format!("{}.dispatch.{endpoint}", self.pipe))
    }

    /// # Errors
    /// Returns a Windows security or mutex creation failure.
    pub fn elect(&self) -> anyhow::Result<Option<InstanceOwner>> {
        let security = Security::new(&self.sid)?;
        let attributes = security.attributes();
        let name = wide(&self.mutex);
        // SAFETY: arguments live through creation; handle ownership is captured below.
        let raw =
            unsafe { CreateMutexW(Some(&raw const attributes), true, PCWSTR(name.as_ptr()))? };
        // SAFETY: GetLastError immediately follows the API whose status it describes.
        let existed = unsafe { GetLastError() } == ERROR_ALREADY_EXISTS;
        let handle = Handle::new(raw)?;
        // SAFETY: zero-time wait acquires an unowned/abandoned mutex without blocking.
        let acquired = !existed
            || matches!(
                unsafe { WaitForSingleObject(handle.raw(), 0) },
                WAIT_OBJECT_0 | WAIT_ABANDONED
            );
        Ok(acquired.then_some(InstanceOwner {
            handle,
            _same_thread: PhantomData,
        }))
    }
}

// Windows mutex ownership is thread-affine. This guard cannot leave the electing
// thread and outlives host join, so a replacement cannot race active share cleanup.
pub struct InstanceOwner {
    handle: Handle,
    _same_thread: PhantomData<Rc<()>>,
}
impl Drop for InstanceOwner {
    fn drop(&mut self) {
        // SAFETY: this thread successfully acquired the mutex in Scope::elect.
        unsafe {
            let _ = ReleaseMutex(self.handle.raw());
        }
    }
}
