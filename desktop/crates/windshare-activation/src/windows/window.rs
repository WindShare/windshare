use windows::Win32::{
    Foundation::HWND,
    UI::WindowsAndMessaging::{
        IsIconic, IsWindowVisible, SW_RESTORE, SW_SHOW, SetForegroundWindow, ShowWindowAsync,
    },
};

/// Presents the application's native window, including a hidden cold launch.
/// Windows validates the handle; callers provide their own GPUI window's HWND.
pub fn present(handle: isize) {
    let hwnd = HWND(handle as *mut std::ffi::c_void);
    // SAFETY: these APIs validate HWND and do not dereference caller-owned memory.
    unsafe {
        if IsIconic(hwnd).as_bool() {
            let _ = ShowWindowAsync(hwnd, SW_RESTORE);
        } else if !IsWindowVisible(hwnd).as_bool() {
            let _ = ShowWindowAsync(hwnd, SW_SHOW);
        }
        let foreground = SetForegroundWindow(hwnd).as_bool();
        tracing::info!(event = "activation.window_presented", foreground);
    }
}
