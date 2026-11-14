use super::handles::wide;
use std::{
    ffi::OsString,
    os::windows::ffi::OsStringExt,
    path::PathBuf,
    sync::{Arc, Mutex},
};
use windows::{
    Win32::{
        Foundation::{ERROR_CANCELLED, HWND, LPARAM, WPARAM},
        System::Com::{CLSCTX_INPROC_SERVER, CoCreateInstance, CoTaskMemFree},
        UI::{
            Shell::{
                FOS_ALLOWMULTISELECT, FOS_FORCEFILESYSTEM, FOS_PICKFOLDERS, FileOpenDialog,
                IFileOpenDialog, SIGDN_FILESYSPATH,
            },
            WindowsAndMessaging::PostMessageW,
        },
    },
    core::{Interface, PCWSTR},
};

#[derive(Default)]
pub(super) struct State {
    pub id: crate::IntentId,
    pub window: Option<isize>,
    pub cancelled: bool,
}
pub struct Picker {
    state: Arc<Mutex<State>>,
}
pub type PickerResult = Result<Option<Vec<PathBuf>>, String>;

impl Picker {
    /// # Errors
    /// Returns a native dialog thread creation failure.
    /// # Panics
    /// The worker panics if its dialog state mutex is poisoned.
    pub fn start(
        owner: isize,
        folders: bool,
        title: String,
    ) -> std::io::Result<(Self, async_channel::Receiver<PickerResult>)> {
        let state = Arc::new(Mutex::new(State::default()));
        let dialog_state = state.clone();
        let (sender, receiver) = async_channel::bounded(1);
        std::thread::Builder::new()
            .name("windshare-picker".into())
            .spawn(move || {
                let result = choose(owner, folders, &title, &dialog_state)
                    .map_err(|error| error.to_string());
                dialog_state.lock().expect("picker state").window = None;
                let _ = sender.send_blocking(result);
            })?;
        Ok((Self { state }, receiver))
    }
}

impl Drop for Picker {
    fn drop(&mut self) {
        let mut state = self.state.lock().expect("picker state");
        state.cancelled = true;
        tracing::info!(event = "activation.picker_cancel_requested", picker_id = %state.id, window = ?state.window);
        if let Some(window) = state.window {
            // SAFETY: only the owned cancellation window is targeted, while the
            // state lock prevents racing destruction and handle reuse.
            unsafe {
                let _ = PostMessageW(
                    Some(HWND(window as *mut _)),
                    super::picker_cancel::CANCEL,
                    WPARAM(0),
                    LPARAM(0),
                );
            }
        }
    }
}

fn choose(
    owner: isize,
    folders: bool,
    title: &str,
    state: &Arc<Mutex<State>>,
) -> anyhow::Result<Option<Vec<PathBuf>>> {
    let _apartment =
        super::handles::Apartment::new(windows::Win32::System::WinRT::RO_INIT_SINGLETHREADED)?;
    // SAFETY: FileOpenDialog is an OS COM class; all access remains on this STA.
    let dialog: IFileOpenDialog =
        unsafe { CoCreateInstance(&FileOpenDialog, None, CLSCTX_INPROC_SERVER)? };
    let _cancellation = super::picker_cancel::Cancellation::new(dialog.cast()?, state.clone())?;
    let title = wide(title);
    // SAFETY: the dialog and cancellation window remain alive through Show.
    unsafe {
        let mut options = FOS_FORCEFILESYSTEM | FOS_ALLOWMULTISELECT;
        if folders {
            options |= FOS_PICKFOLDERS;
        }
        dialog.SetOptions(options)?;
        dialog.SetTitle(PCWSTR(title.as_ptr()))?;
        if state.lock().expect("picker state").cancelled {
            return Ok(None);
        }
        let events: windows::Win32::UI::Shell::IFileDialogEvents =
            super::picker_events::Events(state.clone()).into();
        let cookie = dialog.Advise(&events)?;
        let id = state.lock().expect("picker state").id;
        tracing::info!(event = "activation.picker_showing", picker_id = %id);
        let result = dialog.Show(Some(HWND(owner as *mut _)));
        let _ = dialog.Unadvise(cookie);
        tracing::info!(event = "activation.picker_closed", picker_id = %id, result = ?result);
        if result
            .as_ref()
            .is_err_and(|error| error.code() == ERROR_CANCELLED.to_hresult())
        {
            return Ok(None);
        }
        result?;
        let items = dialog.GetResults()?;
        let count = items.GetCount()?;
        anyhow::ensure!(
            count as usize <= crate::MAX_SELECTED_ROOTS,
            "Too many selected items; select their containing folder"
        );
        let mut paths = Vec::with_capacity(count as usize);
        for index in 0..count {
            let text = items.GetItemAt(index)?.GetDisplayName(SIGDN_FILESYSPATH)?;
            paths.push(PathBuf::from(OsString::from_wide(text.as_wide())));
            CoTaskMemFree(Some(text.0.cast()));
        }
        Ok(Some(paths))
    }
}
