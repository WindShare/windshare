use gpui::{App, FocusHandle, Window};

pub fn advance_focus(scope: &FocusHandle, backwards: bool, window: &mut Window, cx: &mut App) {
    let advance = |window: &mut Window, cx: &mut App| {
        if backwards {
            window.focus_prev(cx);
        } else {
            window.focus_next(cx);
        }
    };
    advance(window, cx);
    let first = window.focused(cx);
    // A dialog shares the native window, so its tab cycle must skip the obscured page.
    while first.is_some() && !scope.contains_focused(window, cx) {
        advance(window, cx);
        if window.focused(cx) == first {
            break;
        }
    }
}
