mod artwork;
mod assets;
mod button;
mod focus;
mod icons;
pub use focus::advance_focus;
mod input;
mod primitives;
mod surface;
mod theme;
mod window_controls;
pub use surface::rounded_surface;

pub use artwork::{VectorArtwork, VectorSprite};
pub use assets::Assets;
pub use button::{Button, ButtonStyle, LARGE_BUTTON_HEIGHT};
pub use icons::{Icon, icon};
pub use input::{Input, InputEvent};
pub use primitives::*;
pub use theme::*;
pub use window_controls::{toggle_window_maximize, window_drag_area};

/// # Panics
/// Panics if the platform text system cannot register the bundled font.
pub fn init(cx: &mut gpui::App) {
    use std::borrow::Cow;
    cx.text_system()
        .add_fonts(vec![Cow::Borrowed(include_bytes!(
            "../assets/fonts/Manrope.ttf"
        ))])
        .expect("bundled Manrope font must load");
}
