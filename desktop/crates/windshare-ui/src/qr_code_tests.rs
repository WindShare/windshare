use super::*;

const PAYLOAD: &str = "https://windshare.example/s/grid";
const FRACTIONAL_SCALE: f32 = 1.5;

fn whole(value: f32) -> bool {
    (value - value.round()).abs() < 1e-3
}

#[test]
fn grid_lands_on_physical_pixels_at_fractional_scale() {
    let code = QrCodeImage::encode(PAYLOAD).unwrap();
    let bounds = Bounds::new(point(px(101.3), px(40.7)), size(px(288.), px(288.)));
    let grid = code.grid(bounds, FRACTIONAL_SCALE);

    assert!(whole(f32::from(grid.cell) * FRACTIONAL_SCALE));
    assert!(whole(f32::from(grid.origin.x) * FRACTIONAL_SCALE));
    assert!(whole(f32::from(grid.origin.y) * FRACTIONAL_SCALE));
    let drawn = grid.cell * f32::from(code.modules_with_margin());
    assert!(grid.origin.x >= bounds.left() - px(1.));
    assert!(grid.origin.x + drawn <= bounds.right() + px(1.));
}

#[test]
fn dark_modules_expand_every_run_inside_the_quiet_zone() {
    let code = QrCodeImage::encode(PAYLOAD).unwrap();
    let modules: Vec<_> = code.dark_modules().collect();
    let expected: usize = code.runs.iter().map(|run| usize::from(run.width)).sum();
    let inner = QUIET_MODULES..code.modules_with_margin() - QUIET_MODULES;

    assert_eq!(modules.len(), expected);
    assert!(
        modules
            .iter()
            .all(|(x, y)| inner.contains(x) && inner.contains(y))
    );
}
