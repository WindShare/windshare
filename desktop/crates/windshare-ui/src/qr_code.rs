use gpui::{Bounds, Div, Pixels, Point, canvas, div, fill, point, prelude::*, px, rgb, size};
use qrcode::{Color, EcLevel, QrCode};
use std::sync::Arc;

pub use qrcode::types::QrError as QrCodeError;

const QUIET_MODULES: u16 = 4;

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

/// Where a code's modules land inside the square it is drawn in.
#[derive(Clone, Copy, Debug)]
pub struct QrGrid {
    pub origin: Point<Pixels>,
    pub cell: Pixels,
}

impl QrGrid {
    pub fn module_center(&self, x: u16, y: u16) -> Point<Pixels> {
        self.origin
            + point(
                self.cell * (f32::from(x) + 0.5),
                self.cell * (f32::from(y) + 0.5),
            )
    }
}

impl QrCodeImage {
    /// Dark themes must preserve the scanner's white quiet zone.
    pub const GROUND: u32 = 0xff_ffff;
    pub const INK: u32 = 0x00_0000;

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

    /// Snaps both module size and origin in physical pixels. Scaling a bitmap
    /// or antialiasing cell edges blurs dense links at fractional Windows DPI.
    /// Every painter of this code shares the grid, so motion that ends on it
    /// lands on exactly the pixels the static view fills.
    pub fn grid(&self, bounds: Bounds<Pixels>, scale_factor: f32) -> QrGrid {
        let modules = f32::from(self.modules);
        let available = f32::from(bounds.size.width.min(bounds.size.height));
        let cell = (available * scale_factor / modules).floor() / scale_factor;
        let drawn = cell * modules;
        let snapped = |start: Pixels, extent: Pixels| {
            px(
                ((f32::from(start) + (f32::from(extent) - drawn) / 2.) * scale_factor).round()
                    / scale_factor,
            )
        };
        QrGrid {
            origin: point(
                snapped(bounds.left(), bounds.size.width),
                snapped(bounds.top(), bounds.size.height),
            ),
            cell: px(cell),
        }
    }

    /// Column and row of every dark module, counted from the quiet zone's corner.
    pub fn dark_modules(&self) -> impl Iterator<Item = (u16, u16)> + '_ {
        self.runs
            .iter()
            .flat_map(|run| (run.x..run.x + run.width).map(move |x| (x, run.y)))
    }

    pub fn view(&self, side: f32) -> Div {
        let code = self.clone();
        div()
            .size(px(side))
            .flex_shrink_0()
            .bg(rgb(Self::GROUND))
            .child(
                canvas(
                    |_, _, _| (),
                    move |bounds, (), window, _| {
                        let grid = code.grid(bounds, window.scale_factor());
                        for run in code.runs.iter() {
                            window.paint_quad(fill(
                                Bounds::new(
                                    grid.origin
                                        + point(
                                            grid.cell * f32::from(run.x),
                                            grid.cell * f32::from(run.y),
                                        ),
                                    size(grid.cell * f32::from(run.width), grid.cell),
                                ),
                                rgb(Self::INK),
                            ));
                        }
                    },
                )
                .size_full(),
            )
    }
}

#[cfg(test)]
#[path = "qr_code_tests.rs"]
mod tests;
