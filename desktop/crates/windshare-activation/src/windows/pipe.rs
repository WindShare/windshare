use super::{
    handles::{Handle, wide},
    security::Security,
};
use crate::protocol::{self, MAX_MESSAGE_BYTES};
use serde::{Serialize, de::DeserializeOwned};
use std::time::Instant;
use windows::{
    Win32::{
        Foundation::{
            ERROR_IO_PENDING, ERROR_OPERATION_ABORTED, ERROR_PIPE_CONNECTED, ERROR_TIMEOUT,
            GENERIC_READ, GENERIC_WRITE, WAIT_OBJECT_0, WAIT_TIMEOUT,
        },
        Storage::FileSystem::{
            CreateFileW, FILE_FLAG_FIRST_PIPE_INSTANCE, FILE_FLAG_OVERLAPPED, FILE_SHARE_NONE,
            OPEN_EXISTING, PIPE_ACCESS_DUPLEX, ReadFile, SECURITY_IDENTIFICATION,
            SECURITY_SQOS_PRESENT, WriteFile,
        },
        System::{
            IO::{CancelIoEx, GetOverlappedResult, OVERLAPPED},
            Pipes::{
                ConnectNamedPipe, CreateNamedPipeW, PIPE_READMODE_BYTE, PIPE_REJECT_REMOTE_CLIENTS,
                PIPE_TYPE_BYTE, PIPE_WAIT,
            },
            Threading::{CreateEventW, WaitForSingleObject},
        },
    },
    core::PCWSTR,
};

const PIPE_BUFFER_BYTES: u32 = 64 * 1024;

pub struct Pipe {
    handle: Handle,
}
impl Pipe {
    pub fn listen(name: &str, sid: &str) -> anyhow::Result<Self> {
        let security = Security::new(sid)?;
        let attributes = security.attributes();
        let name = wide(name);
        // SAFETY: both string and security descriptor outlive the synchronous call.
        let handle = unsafe {
            CreateNamedPipeW(
                PCWSTR(name.as_ptr()),
                PIPE_ACCESS_DUPLEX | FILE_FLAG_OVERLAPPED | FILE_FLAG_FIRST_PIPE_INSTANCE,
                PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT | PIPE_REJECT_REMOTE_CLIENTS,
                1,
                PIPE_BUFFER_BYTES,
                PIPE_BUFFER_BYTES,
                0,
                Some(&raw const attributes),
            )
        };
        Ok(Self {
            handle: Handle::new(handle)?,
        })
    }

    pub fn connect(name: &str) -> anyhow::Result<Self> {
        let name = wide(name);
        // Identification prevents a same-user server from impersonating the caller.
        // SAFETY: the pipe path is terminated and no borrowed handle is retained.
        let handle = unsafe {
            CreateFileW(
                PCWSTR(name.as_ptr()),
                (GENERIC_READ | GENERIC_WRITE).0,
                FILE_SHARE_NONE,
                None,
                OPEN_EXISTING,
                FILE_FLAG_OVERLAPPED | SECURITY_SQOS_PRESENT | SECURITY_IDENTIFICATION,
                None,
            )?
        };
        Ok(Self {
            handle: Handle::new(handle)?,
        })
    }

    pub fn accept(&self, deadline: Instant) -> windows::core::Result<()> {
        match self.perform(deadline, |overlapped| {
            // SAFETY: perform keeps the OVERLAPPED alive through completion/cancellation.
            unsafe { ConnectNamedPipe(self.handle.raw(), Some(overlapped)) }
        }) {
            // A client can connect before accept; no overlapped operation exists then.
            Err(error) if error.code() == ERROR_PIPE_CONNECTED.to_hresult() => Ok(()),
            result => result.map(|_| ()),
        }
    }

    pub fn send<T: Serialize>(&self, value: &T, deadline: Instant) -> anyhow::Result<()> {
        let bytes = protocol::encode(value).map_err(anyhow::Error::msg)?;
        self.write(&u32::try_from(bytes.len())?.to_le_bytes(), deadline)?;
        self.write(&bytes, deadline)
    }

    pub fn receive<T: DeserializeOwned>(&self, deadline: Instant) -> anyhow::Result<T> {
        let mut length = [0; 4];
        self.read(&mut length, deadline)?;
        let length = u32::from_le_bytes(length) as usize;
        anyhow::ensure!(
            length <= MAX_MESSAGE_BYTES,
            "Activation message is too large"
        );
        let mut bytes = vec![0; length];
        self.read(&mut bytes, deadline)?;
        protocol::decode(&bytes).map_err(anyhow::Error::msg)
    }

    pub fn client_pid(&self) -> anyhow::Result<u32> {
        let mut pid = 0;
        // SAFETY: this is a connected server pipe and a valid output pointer.
        unsafe {
            windows::Win32::System::Pipes::GetNamedPipeClientProcessId(
                self.handle.raw(),
                &raw mut pid,
            )?;
        }
        Ok(pid)
    }

    pub fn allow_server_foreground(&self) {
        let mut pid = 0;
        // SAFETY: this is a connected client pipe and a writable PID output.
        let granted = unsafe {
            windows::Win32::System::Pipes::GetNamedPipeServerProcessId(
                self.handle.raw(),
                &raw mut pid,
            )
            .and_then(|()| windows::Win32::UI::WindowsAndMessaging::AllowSetForegroundWindow(pid))
        };
        tracing::debug!(
            event = "activation.foreground_permission",
            pid,
            granted = granted.is_ok()
        );
    }

    fn read(&self, mut bytes: &mut [u8], deadline: Instant) -> anyhow::Result<()> {
        while !bytes.is_empty() {
            let read = self.perform(deadline, |overlapped| {
                // SAFETY: the slice and OVERLAPPED live until IO has settled.
                unsafe { ReadFile(self.handle.raw(), Some(bytes), None, Some(overlapped)) }
            })?;
            anyhow::ensure!(read != 0, "Activation pipe closed");
            bytes = &mut bytes[read..];
        }
        Ok(())
    }

    fn write(&self, mut bytes: &[u8], deadline: Instant) -> anyhow::Result<()> {
        while !bytes.is_empty() {
            let written = self.perform(deadline, |overlapped| {
                // SAFETY: the slice and OVERLAPPED live until IO has settled.
                unsafe { WriteFile(self.handle.raw(), Some(bytes), None, Some(overlapped)) }
            })?;
            anyhow::ensure!(written != 0, "Activation pipe closed");
            bytes = &bytes[written..];
        }
        Ok(())
    }

    fn perform(
        &self,
        deadline: Instant,
        start: impl FnOnce(*mut OVERLAPPED) -> windows::core::Result<()>,
    ) -> windows::core::Result<usize> {
        // SAFETY: creates an unnamed, owned manual-reset event.
        let event = Handle::new(unsafe { CreateEventW(None, true, false, None)? })?;
        let mut overlapped = OVERLAPPED {
            hEvent: event.raw(),
            ..Default::default()
        };
        let result = start(&raw mut overlapped);
        if let Err(error) = &result
            && error.code() != ERROR_IO_PENDING.to_hresult()
        {
            return Err(error.clone());
        }
        let mut transferred = 0;
        if result.is_err() {
            let remaining = u32::try_from(
                deadline
                    .saturating_duration_since(Instant::now())
                    .as_millis(),
            )
            .unwrap_or(u32::MAX - 1);
            // SAFETY: the event and OVERLAPPED are retained until the kernel completes.
            let wait = unsafe { WaitForSingleObject(event.raw(), remaining) };
            if wait != WAIT_OBJECT_0 {
                let error = if wait == WAIT_TIMEOUT {
                    windows::core::Error::from_hresult(ERROR_TIMEOUT.to_hresult())
                } else {
                    windows::core::Error::from_thread()
                };
                // Cancellation must settle before stack buffers can be released.
                // SAFETY: both operations refer to this still-live pending request.
                let completion = unsafe {
                    let _ = CancelIoEx(self.handle.raw(), Some(&raw const overlapped));
                    GetOverlappedResult(
                        self.handle.raw(),
                        &raw const overlapped,
                        &raw mut transferred,
                        true,
                    )
                };
                if wait == WAIT_TIMEOUT {
                    // Completion can beat cancellation. Preserve an accepted client or
                    // transferred bytes, and never hide a broken pipe as an idle poll.
                    match completion {
                        Ok(()) => return Ok(transferred as usize),
                        Err(failure) if failure.code() != ERROR_OPERATION_ABORTED.to_hresult() => {
                            return Err(failure);
                        }
                        Err(_) => {}
                    }
                }
                return Err(error);
            }
        }
        // SAFETY: the event signalled or the IO completed synchronously.
        unsafe {
            GetOverlappedResult(
                self.handle.raw(),
                &raw const overlapped,
                &raw mut transferred,
                false,
            )?;
        }
        Ok(transferred as usize)
    }
}
