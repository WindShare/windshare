use super::*;

pub(super) fn text_canvas(input: Entity<Input>) -> impl IntoElement {
    let layout_input = input.clone();
    canvas(
        move |bounds, window, cx| {
            let theme = Theme::from(window.appearance());
            let field = layout_input.read(cx);
            let style = window.text_style();
            let text: SharedString = if field.value.is_empty() {
                field.placeholder.clone()
            } else {
                field.value.clone().into()
            };
            let run = TextRun {
                len: text.len(),
                font: style.font(),
                color: if field.value.is_empty() {
                    theme.muted_text()
                } else {
                    style.color
                },
                background_color: None,
                underline: None,
                strikethrough: None,
            };
            let line = window
                .text_system()
                .shape_line(text, px(INPUT_FONT_SIZE), &[run], None);
            let caret = line.x_for_index(field.cursor);
            let scroll = if field.value.is_empty() {
                px(0.)
            } else {
                field
                    .scroll
                    .max(caret - bounds.size.width + px(2.))
                    .min(caret)
                    .max(px(0.))
            };
            (line, scroll)
        },
        move |bounds, (line, scroll), window, cx| {
            let theme = Theme::from(window.appearance());
            let field = input.read(cx);
            let selection = field.selection();
            let focused = field.focus.is_focused(window);
            let cursor = field.cursor;
            let marked = field.marked.clone();
            let origin = point(bounds.left() - scroll, bounds.top());
            window.handle_input(
                &field.focus,
                ElementInputHandler::new(bounds, input.clone()),
                cx,
            );
            if focused && !selection.is_empty() {
                window.paint_quad(fill(
                    Bounds::from_corners(
                        point(origin.x + line.x_for_index(selection.start), bounds.top()),
                        point(origin.x + line.x_for_index(selection.end), bounds.bottom()),
                    ),
                    theme.selection(),
                ));
            }
            let _ = line.paint(
                origin,
                px(INPUT_LINE_HEIGHT),
                TextAlign::Left,
                None,
                window,
                cx,
            );
            if focused && selection.is_empty() {
                window.paint_quad(fill(
                    Bounds::new(
                        point(origin.x + line.x_for_index(cursor), bounds.top() + px(2.)),
                        size(px(1.5), px(22.)),
                    ),
                    theme.accent_text(),
                ));
            }
            if let Some(marked) = &marked {
                window.paint_quad(fill(
                    Bounds::from_corners(
                        point(
                            origin.x + line.x_for_index(marked.start),
                            bounds.bottom() - px(1.),
                        ),
                        point(origin.x + line.x_for_index(marked.end), bounds.bottom()),
                    ),
                    theme.accent_text(),
                ));
            }
            input.update(cx, |field, _| {
                field.layout = Some(line);
                field.bounds = Some(bounds);
                field.scroll = scroll;
            });
        },
    )
    .w_full()
    .h(px(INPUT_LINE_HEIGHT))
}
