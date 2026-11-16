use std::{io::Write, path::PathBuf};

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
    for side in [44_u16, 50, 150, 256] {
        let mut pixmap = resvg::tiny_skia::Pixmap::new(u32::from(side), u32::from(side))
            .ok_or_else(|| anyhow::anyhow!("Invalid asset size"))?;
        let scale = f32::from(side) / tree.size().width();
        resvg::render(
            &tree,
            resvg::tiny_skia::Transform::from_scale(scale, scale),
            &mut pixmap.as_mut(),
        );
        let png = pixmap.encode_png()?;
        std::fs::write(directory.join(format!("logo-{side}.png")), &png)?;
        if side == 256 {
            let mut icon = std::fs::File::create(directory.join("windshare.ico"))?;
            icon.write_all(&[0, 0, 1, 0, 1, 0, 0, 0, 0, 0, 1, 0, 32, 0])?;
            icon.write_all(&u32::try_from(png.len())?.to_le_bytes())?;
            icon.write_all(&22_u32.to_le_bytes())?;
            icon.write_all(&png)?;
        }
    }
    Ok(())
}
