use super::visuals::Finish;
use gpui::{
    Bounds, Div, Path, PathBuilder, Pixels, canvas, div, linear_color_stop, linear_gradient, point,
    prelude::*, px,
};
use windshare_ui::Theme;

pub fn backdrop(theme: Theme) -> Div {
    let finish = Finish::new(theme);
    // One window-sized painting keeps the sidebar, titlebar and content on the same surface.
    div()
        .absolute()
        .inset_0()
        .size_full()
        .overflow_hidden()
        .bg(linear_gradient(
            125.,
            linear_color_stop(finish.pearl, 0.),
            linear_color_stop(finish.sky, 1.),
        ))
        .child(
            canvas(
                |bounds, _, _| silk_paths(bounds),
                move |_, paths, window, _| {
                    let fills = [
                        linear_gradient(
                            160.,
                            linear_color_stop(finish.blush.opacity(0.72), 0.),
                            linear_color_stop(finish.pearl.opacity(0.1), 1.),
                        ),
                        linear_gradient(
                            135.,
                            linear_color_stop(finish.mint.opacity(0.26), 0.),
                            linear_color_stop(finish.mint, 1.),
                        ),
                        linear_gradient(
                            120.,
                            linear_color_stop(finish.pearl.opacity(0.05), 0.),
                            linear_color_stop(finish.pearl.opacity(0.72), 1.),
                        ),
                        linear_gradient(
                            145.,
                            linear_color_stop(finish.foil.opacity(0.02), 0.),
                            linear_color_stop(finish.foil.opacity(0.22), 1.),
                        ),
                    ];
                    for (path, fill) in paths.into_iter().zip(fills) {
                        window.paint_path(path, fill);
                    }
                },
            )
            .absolute()
            .inset_0()
            .size_full(),
        )
}

fn silk_paths(bounds: Bounds<Pixels>) -> [Path<Pixels>; 4] {
    let at = |x: f32, y: f32| bounds.origin + point(bounds.size.width * x, bounds.size.height * y);
    let mut warmth = PathBuilder::fill();
    warmth.move_to(at(-0.1, -0.1));
    warmth.line_to(at(0.72, -0.1));
    warmth.cubic_bezier_to(at(-0.1, 0.72), at(0.59, 0.30), at(0.32, 0.67));
    warmth.close();

    let mut silk = PathBuilder::fill();
    silk.move_to(at(0.11, 1.12));
    silk.cubic_bezier_to(at(1.10, -0.12), at(1.12, 0.89), at(0.47, 0.32));
    silk.line_to(at(1.12, 1.12));
    silk.close();

    let mut fold = PathBuilder::fill();
    fold.move_to(at(0.33, 1.12));
    fold.cubic_bezier_to(at(1.10, -0.12), at(1.10, 0.82), at(0.53, 0.30));
    fold.line_to(at(1.14, 0.04));
    fold.cubic_bezier_to(at(0.68, 1.14), at(0.75, 0.42), at(1.14, 0.80));
    fold.close();

    let mut seam = PathBuilder::stroke(px(0.8));
    seam.move_to(at(0.22, 1.12));
    seam.cubic_bezier_to(at(1.10, -0.12), at(1.13, 0.88), at(0.48, 0.31));
    [warmth, silk, fold, seam].map(|builder| builder.build().expect("silk curves are valid"))
}
