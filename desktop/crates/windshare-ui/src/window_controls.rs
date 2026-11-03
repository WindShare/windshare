use gpui::{Div, MouseButton, Window, WindowControlArea, div, prelude::*};

pub fn window_drag_area() -> Div {
    div()
        .window_control_area(WindowControlArea::Drag)
        // Windows owns caption dragging and double-clicks through the hit-test
        // region. Handling them again in GPUI can issue a second window command.
        .when(cfg!(not(target_os = "windows")), |el| {
            el.on_mouse_down(MouseButton::Left, |event, window, _| {
                if event.click_count == 2 {
                    toggle_window_maximize(window);
                } else {
                    window.start_window_move();
                }
            })
        })
}

pub fn toggle_window_maximize(window: &Window) {
    let maximized = window.is_maximized();
    tracing::info!(
        operation = "desktop.window.maximize_requested",
        session = std::process::id(),
        window = window.window_handle().window_id().as_u64(),
        maximized,
        target_maximized = !maximized,
    );

    #[cfg(target_os = "windows")]
    if let Err(error) = request_windows_maximize(window, maximized) {
        tracing::error!(
            operation = "desktop.window.maximize_failed",
            session = std::process::id(),
            window = window.window_handle().window_id().as_u64(),
            maximized,
            error = %error,
        );
    }

    #[cfg(not(target_os = "windows"))]
    window.zoom_window();
}

#[cfg(target_os = "windows")]
fn request_windows_maximize(window: &Window, maximized: bool) -> anyhow::Result<()> {
    use raw_window_handle::{HasWindowHandle, RawWindowHandle};
    use windows::Win32::{
        Foundation::{HWND, LPARAM, WPARAM},
        UI::WindowsAndMessaging::{PostMessageW, SC_MAXIMIZE, SC_RESTORE, WM_SYSCOMMAND},
    };

    let handle = HasWindowHandle::window_handle(window)?;
    let RawWindowHandle::Win32(handle) = handle.as_raw() else {
        anyhow::bail!("expected a Win32 window handle");
    };
    let command = if maximized { SC_RESTORE } else { SC_MAXIMIZE };
    // GPUI 0.3.11's Windows zoom() only maximizes. Native system commands retain
    // the OS-owned restore placement, including changes made outside our button.
    // Post instead of Send to avoid reentering GPUI during an input callback.
    // SAFETY: the handle is borrowed from this live window; the message has no pointers.
    unsafe {
        PostMessageW(
            Some(HWND(handle.hwnd.get() as *mut std::ffi::c_void)),
            WM_SYSCOMMAND,
            WPARAM(command as usize),
            LPARAM::default(),
        )?;
    }
    Ok(())
}
