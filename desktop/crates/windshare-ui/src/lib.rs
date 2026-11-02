mod artwork;
mod assets;
mod button;
mod focus;
mod icons;
pub use focus::advance_focus;
mod input;
mod primitives;
mod theme;

pub use artwork::VectorArtwork;
pub use assets::Assets;
pub use button::{Button, ButtonStyle};
pub use icons::{Icon, icon};
pub use input::{Input, InputEvent};
pub use primitives::*;
pub use theme::*;

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
