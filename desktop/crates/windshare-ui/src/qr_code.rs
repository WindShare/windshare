use gpui::{Bounds, Div, canvas, div, fill, point, prelude::*, px, rgb, size};
use qrcode::{Color, EcLevel, QrCode};
use std::sync::Arc;

pub use qrcode::types::QrError as QrCodeError;

const QUIET_MODULES: u16 = 4;
const QR_BACKGROUND: u32 = 0xff_ffff;
const QR_FOREGROUND: u32 = 0x00_0000;

#[derive(Clone, Copy)]
struct ModuleRun {
    x: u16,
    y: u16,
    width: u16,
}

/// Encoded once, then shared by inline and enlarged views without retaining a raster size.
#[derive(Clone)]
pub struct QrCodeImage {
    modules: u16,
    runs: Arc<[ModuleRun]>,
}

impl QrCodeImage {
    /// # Errors
    /// Returns the encoder error if the complete payload cannot fit a QR code.
    pub fn encode(payload: &str) -> Result<Self, QrCodeError> {
        let code = QrCode::with_error_correction_level(payload.as_bytes(), EcLevel::M)?;
        let width = u16::try_from(code.width()).map_err(|_| QrCodeError::InvalidVersion)?;
        let mut runs = Vec::new();
        for y in 0..width {
            let mut x = 0;
            while x < width {
                if code[(usize::from(x), usize::from(y))] == Color::Light {
                    x += 1;
                    continue;
                }
                let start = x;
                while x < width && code[(usize::from(x), usize::from(y))] == Color::Dark {
                    x += 1;
                }
                runs.push(ModuleRun {
                    x: start + QUIET_MODULES,
                    y: y + QUIET_MODULES,
                    width: x - start,
                });
            }
        }
        Ok(Self {
            modules: width + QUIET_MODULES * 2,
            runs: runs.into(),
        })
    }

    pub const fn modules_with_margin(&self) -> u16 {
        self.modules
    }

    pub fn side_for_modules(&self, module_side: f32) -> f32 {
        f32::from(self.modules) * module_side
    }

    pub fn view(&self, side: f32) -> Div {
        let modules = f32::from(self.modules);
        let runs = Arc::clone(&self.runs);
        div()
            .size(px(side))
            .flex_shrink_0()
            // Dark themes must preserve the scanner's white quiet zone.
            .bg(rgb(QR_BACKGROUND))
            .child(
                canvas(
                    |_, _, _| (),
                    move |bounds, (), window, _| {
                        let scale = window.scale_factor();
                        let available = f32::from(bounds.size.width.min(bounds.size.height));
                        let cell = (available * scale / modules).floor() / scale;
                        let drawn = cell * modules;
                        // Snap both module size and origin in physical pixels. Scaling a bitmap
                        // or antialiasing cell edges blurs dense links at fractional Windows DPI.
                        let origin = point(
                            px(((f32::from(bounds.left())
                                + (f32::from(bounds.size.width) - drawn) / 2.)
                                * scale)
                                .round()
                                / scale),
                            px(((f32::from(bounds.top())
                                + (f32::from(bounds.size.height) - drawn) / 2.)
                                * scale)
                                .round()
                                / scale),
                        );
                        for run in runs.iter() {
                            window.paint_quad(fill(
                                Bounds::new(
                                    origin
                                        + point(
                                            px(f32::from(run.x) * cell),
                                            px(f32::from(run.y) * cell),
                                        ),
                                    size(px(f32::from(run.width) * cell), px(cell)),
                                ),
                                rgb(QR_FOREGROUND),
                            ));
                        }
                    },
                )
                .size_full(),
            )
    }
}
