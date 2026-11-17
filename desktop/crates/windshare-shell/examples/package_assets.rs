use std::{
    io::Write,
    path::{Path, PathBuf},
};

const PACKAGE_LOGO_SIZES: [u16; 3] = [44, 50, 150];
const APP_ICON_SIZES: [u16; 14] = [16, 20, 24, 30, 32, 36, 40, 48, 60, 64, 72, 80, 96, 256];
const SHELL_ICON_SIZES: [u16; 15] = [16, 20, 24, 28, 30, 32, 36, 40, 48, 60, 64, 72, 80, 96, 256];
const APP_ICON_FORMS: [&str; 3] = ["", "_altform-unplated", "_altform-lightunplated"];
const ICO_MAX_SIDE: u16 = 256;
const ICO_HEADER_BYTES: u32 = 6;
const ICO_ENTRY_BYTES: u32 = 16;

fn main() -> anyhow::Result<()> {
    let directory = PathBuf::from(
        std::env::args_os()
            .nth(1)
            .ok_or_else(|| anyhow::anyhow!("Expected asset output directory"))?,
    );
    std::fs::create_dir_all(&directory)?;
    let tree = resvg::usvg::Tree::from_data(
        include_bytes!("../../windshare-desktop/assets/spirit-mark.svg"),
        &resvg::usvg::Options::default(),
    )?;
    for side in PACKAGE_LOGO_SIZES {
        std::fs::write(
            directory.join(format!("logo-{side}.png")),
            render_png(&tree, side)?,
        )?;
    }
    // A transparent base PNG alone still gets a system backplate. Start and the
    // taskbar need explicit unplated candidates for both light and dark themes.
    for side in APP_ICON_SIZES {
        let png = render_png(&tree, side)?;
        for form in APP_ICON_FORMS {
            std::fs::write(
                directory.join(format!("logo-44.targetsize-{side}{form}.png")),
                &png,
            )?;
        }
    }
    // Explorer needs the same simple silhouette at every DPI, independently of
    // the larger mascot artwork used by Start and the taskbar.
    let symbol = resvg::usvg::Tree::from_data(
        include_bytes!("../../windshare-desktop/assets/spirit-symbol.svg"),
        &resvg::usvg::Options::default(),
    )?;
    write_icon(&directory.join("windshare.ico"), &symbol)?;
    Ok(())
}

fn render_png(tree: &resvg::usvg::Tree, side: u16) -> anyhow::Result<Vec<u8>> {
    let mut pixmap = resvg::tiny_skia::Pixmap::new(u32::from(side), u32::from(side))
        .ok_or_else(|| anyhow::anyhow!("Invalid asset size"))?;
    let scale = f32::from(side) / tree.size().width();
    resvg::render(
        tree,
        resvg::tiny_skia::Transform::from_scale(scale, scale),
        &mut pixmap.as_mut(),
    );
    Ok(pixmap.encode_png()?)
}

fn write_icon(path: &Path, tree: &resvg::usvg::Tree) -> anyhow::Result<()> {
    // Native-size frames avoid Explorer resampling a single large raster at
    // fractional display scales, where eyes and wind tips lose their definition.
    let frames = SHELL_ICON_SIZES
        .into_iter()
        .map(|side| Ok((side, render_png(tree, side)?)))
        .collect::<anyhow::Result<Vec<_>>>()?;
    let count = u16::try_from(frames.len())?;
    let mut offset = ICO_HEADER_BYTES + ICO_ENTRY_BYTES * u32::from(count);
    let mut icon = std::fs::File::create(path)?;
    icon.write_all(&[0, 0, 1, 0])?;
    icon.write_all(&count.to_le_bytes())?;
    for (side, png) in &frames {
        // ICO encodes a 256-pixel dimension as zero in its one-byte field.
        let dimension = if *side == ICO_MAX_SIDE {
            0
        } else {
            u8::try_from(*side)?
        };
        let length = u32::try_from(png.len())?;
        icon.write_all(&[dimension, dimension, 0, 0, 1, 0, 32, 0])?;
        icon.write_all(&length.to_le_bytes())?;
        icon.write_all(&offset.to_le_bytes())?;
        offset += length;
    }
    for (_, png) in frames {
        icon.write_all(&png)?;
    }
    Ok(())
}
