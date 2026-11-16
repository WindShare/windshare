use super::picker::State;
use std::sync::{Arc, Mutex, OnceLock};
use windows::{
    Win32::{
        Foundation::{ERROR_CLASS_ALREADY_EXISTS, HWND, LPARAM, LRESULT, WPARAM},
        System::{LibraryLoader::GetModuleHandleW, Ole::IOleWindow},
        UI::{
            Shell::IFileDialog,
            WindowsAndMessaging::{
                CreateWindowExW, DefWindowProcW, DestroyWindow, GWLP_USERDATA, GetWindowLongPtrW,
                HWND_MESSAGE, RegisterClassW, SendMessageW, SetWindowLongPtrW, WINDOW_EX_STYLE,
                WINDOW_STYLE, WM_APP, WM_CLOSE, WNDCLASSW,
            },
        },
    },
    core::{Interface, w},
};

pub(super) const CANCEL: u32 = WM_APP + 0x41;
static CLASS: OnceLock<Result<(), String>> = OnceLock::new();

struct Binding {
    dialog: IFileDialog,
    state: Arc<Mutex<State>>,
}

/// An owned STA message window avoids targeting a shell HWND after it is destroyed.
pub(super) struct Cancellation {
    window: HWND,
    binding: Box<Binding>,
}

impl Cancellation {
    pub fn new(dialog: IFileDialog, state: Arc<Mutex<State>>) -> anyhow::Result<Self> {
        // SAFETY: this process module remains loaded for every picker.
        let module = unsafe { GetModuleHandleW(None)? };
        CLASS
            .get_or_init(|| {
                let class = WNDCLASSW {
                    hInstance: module.into(),
                    lpfnWndProc: Some(window_proc),
                    lpszClassName: w!("WindShare.PickerCancellation"),
                    ..Default::default()
                };
                // SAFETY: this static procedure and class name live through process exit.
                if unsafe { RegisterClassW(&raw const class) } == 0 {
                    let error = windows::core::Error::from_thread();
                    if error.code() != ERROR_CLASS_ALREADY_EXISTS.to_hresult() {
                        return Err(error.to_string());
                    }
                }
                Ok(())
            })
            .as_ref()
            .map_err(|error| anyhow::anyhow!(error.clone()))?;
        let binding = Box::new(Binding { dialog, state });
        // SAFETY: no messages use the binding until it is published below.
        let window = unsafe {
            CreateWindowExW(
                WINDOW_EX_STYLE::default(),
                w!("WindShare.PickerCancellation"),
                None,
                WINDOW_STYLE::default(),
                0,
                0,
                0,
                0,
                Some(HWND_MESSAGE),
                None,
                Some(module.into()),
                None,
            )?
        };
        // SAFETY: the boxed binding has a stable address and outlives the window.
        unsafe {
            SetWindowLongPtrW(
                window,
                GWLP_USERDATA,
                std::ptr::from_ref(&*binding) as isize,
            );
        }
        binding.state.lock().expect("picker state").window = Some(window.0 as isize);
        Ok(Self { window, binding })
    }
}
impl Drop for Cancellation {
    fn drop(&mut self) {
        let mut state = self.binding.state.lock().expect("picker state");
        state.window = None;
        // SAFETY: destruction is on the owning STA, serialized against cancellation posts.
        // WM_DESTROY does not call back into the dialog or take the state lock.
        unsafe {
            let _ = DestroyWindow(self.window);
        }
        drop(state);
    }
}

unsafe extern "system" fn window_proc(window: HWND, message: u32, w: WPARAM, l: LPARAM) -> LRESULT {
    // SAFETY: this procedure belongs only to the message window above; its boxed
    // binding remains alive through DestroyWindow and all dispatched messages.
    unsafe {
        if message == CANCEL {
            let binding = GetWindowLongPtrW(window, GWLP_USERDATA) as *const Binding;
            if let Some(binding) = binding.as_ref() {
                let cancelled = binding.state.lock().expect("picker state").cancelled;
                if cancelled {
                    // Close only works from IFileDialogEvents. Resolve and close the
                    // live shell window synchronously on its own STA instead; no
                    // borrowed shell HWND crosses threads or survives a message turn.
                    match binding
                        .dialog
                        .cast::<IOleWindow>()
                        .and_then(|dialog| dialog.GetWindow())
                    {
                        Ok(dialog) => {
                            tracing::info!(event = "activation.picker_cancel_delivered", window = ?dialog);
                            SendMessageW(dialog, WM_CLOSE, Some(WPARAM(0)), Some(LPARAM(0)));
                        }
                        Err(error) => {
                            tracing::debug!(event = "activation.picker_cancel_before_window", %error);
                        }
                    }
                }
            }
            return LRESULT(0);
        }
        DefWindowProcW(window, message, w, l)
    }
}
