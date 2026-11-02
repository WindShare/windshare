mod editing;
mod paint;

use crate::{INPUT_FONT_SIZE, INPUT_LINE_HEIGHT, border, ink, paper, primary};
use gpui::{
    App, Bounds, ClipboardItem, ElementInputHandler, Entity, EntityInputHandler, EventEmitter,
    FocusHandle, Focusable, KeyDownEvent, MouseButton, MouseDownEvent, MouseMoveEvent, Pixels,
    Point, Role, ShapedLine, SharedString, TextAlign, TextRun, UTF16Selection, Window, canvas, div,
    fill, point, prelude::*, px, size,
};
use std::ops::Range;
use unicode_segmentation::UnicodeSegmentation;

#[derive(Clone, Copy)]
pub enum InputEvent {
    Changed,
    Submit,
}

pub struct Input {
    focus: FocusHandle,
    value: String,
    placeholder: SharedString,
    anchor: usize,
    cursor: usize,
    marked: Option<Range<usize>>,
    layout: Option<ShapedLine>,
    bounds: Option<Bounds<Pixels>>,
    scroll: Pixels,
    selecting: bool,
}

impl EventEmitter<InputEvent> for Input {}
impl Focusable for Input {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus.clone()
    }
}

impl Input {
    pub fn new(placeholder: impl Into<SharedString>, cx: &mut Context<Self>) -> Self {
        Self {
            focus: cx.focus_handle(),
            value: String::new(),
            placeholder: placeholder.into(),
            anchor: 0,
            cursor: 0,
            marked: None,
            layout: None,
            bounds: None,
            scroll: px(0.),
            selecting: false,
        }
    }
    pub fn value(&self) -> &str {
        &self.value
    }
    pub fn set_value(&mut self, value: impl Into<String>, cx: &mut Context<Self>) {
        self.value = value.into();
        self.cursor = self.value.len();
        self.anchor = self.cursor;
        self.marked = None;
        cx.emit(InputEvent::Changed);
        cx.notify();
    }
    pub fn focus(&self, window: &mut Window, cx: &mut App) {
        window.focus(&self.focus, cx);
    }
    fn selection(&self) -> Range<usize> {
        self.anchor.min(self.cursor)..self.anchor.max(self.cursor)
    }
    fn to_utf16(&self, offset: usize) -> usize {
        self.value[..offset].encode_utf16().count()
    }
    fn byte_offset(&self, offset: usize) -> usize {
        let mut units = 0;
        for (index, ch) in self.value.char_indices() {
            if units + ch.len_utf16() > offset {
                return index;
            }
            units += ch.len_utf16();
        }
        self.value.len()
    }
    fn utf16_range(&self, range: Range<usize>) -> Range<usize> {
        self.to_utf16(range.start)..self.to_utf16(range.end)
    }
    fn byte_range(&self, range: Range<usize>) -> Range<usize> {
        self.byte_offset(range.start)..self.byte_offset(range.end)
    }
    fn replace(
        &mut self,
        range: Range<usize>,
        text: &str,
        window: &Window,
        cx: &mut Context<Self>,
    ) {
        let text = text.replace(['\r', '\n'], " ");
        self.value.replace_range(range.clone(), &text);
        self.cursor = range.start + text.len();
        self.anchor = self.cursor;
        self.marked = None;
        window.notify_text_input_changed(&self.focus);
        cx.emit(InputEvent::Changed);
        cx.notify();
    }
    fn previous(&self) -> usize {
        self.value[..self.cursor]
            .grapheme_indices(true)
            .next_back()
            .map_or(0, |(i, _)| i)
    }
    fn next(&self) -> usize {
        self.value[self.cursor..]
            .grapheme_indices(true)
            .nth(1)
            .map_or(self.value.len(), |(i, _)| self.cursor + i)
    }
    fn index_at(&self, position: Point<Pixels>) -> usize {
        match (&self.layout, self.bounds) {
            (Some(line), Some(bounds)) if !self.value.is_empty() => line
                .closest_index_for_x(position.x - bounds.left() + self.scroll)
                .min(self.value.len()),
            _ => 0,
        }
    }
    fn key(&mut self, event: &KeyDownEvent, window: &mut Window, cx: &mut Context<Self>) {
        let key = &event.keystroke;
        let command = key.modifiers.control || key.modifiers.platform;
        let range = self.selection();
        if command {
            match key.key.as_str() {
                "a" => {
                    self.anchor = 0;
                    self.cursor = self.value.len();
                }
                "c" | "x" => {
                    if !range.is_empty() {
                        cx.write_to_clipboard(ClipboardItem::new_string(
                            self.value[range.clone()].into(),
                        ));
                        if key.key == "x" {
                            self.replace(range, "", window, cx);
                        }
                    }
                }
                "v" => {
                    if let Some(text) = cx.read_from_clipboard().and_then(|item| item.text()) {
                        self.replace(range, &text, window, cx);
                    }
                }
                _ => return,
            }
        } else {
            match key.key.as_str() {
                "left" | "right" | "home" | "end" => {
                    self.cursor = match key.key.as_str() {
                        "left" if !key.modifiers.shift && !range.is_empty() => range.start,
                        "right" if !key.modifiers.shift && !range.is_empty() => range.end,
                        "left" => self.previous(),
                        "right" => self.next(),
                        "home" => 0,
                        _ => self.value.len(),
                    };
                    if !key.modifiers.shift {
                        self.anchor = self.cursor;
                    }
                }
                "backspace" => {
                    let range = if range.is_empty() {
                        self.previous()..self.cursor
                    } else {
                        range
                    };
                    self.replace(range, "", window, cx);
                }
                "delete" => {
                    let range = if range.is_empty() {
                        self.cursor..self.next()
                    } else {
                        range
                    };
                    self.replace(range, "", window, cx);
                }
                "enter" => cx.emit(InputEvent::Submit),
                _ => return,
            }
        }
        cx.stop_propagation();
        window.notify_text_selection_changed(&self.focus);
        cx.notify();
    }
}

impl Render for Input {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .id("input")
            .role(Role::TextInput)
            .aria_label(self.placeholder.clone())
            .aria_placeholder(self.placeholder.clone())
            .aria_value(self.value.clone())
            .w_full()
            .min_w_0()
            .h(px(52.))
            .px(px(12.))
            .py(px(12.))
            .bg(paper())
            .rounded(px(10.))
            .border_1()
            .border_color(border())
            .text_size(px(INPUT_FONT_SIZE))
            .text_color(ink())
            .line_height(px(INPUT_LINE_HEIGHT))
            .track_focus(&self.focus)
            .tab_index(0)
            .cursor_text()
            .focus(|s| s.border_color(primary()))
            .overflow_hidden()
            .on_key_down(cx.listener(Self::key))
            .on_mouse_down(
                MouseButton::Left,
                cx.listener(|this, event: &MouseDownEvent, window, cx| {
                    window.focus(&this.focus, cx);
                    let index = this.index_at(event.position);
                    this.cursor = index;
                    if !event.modifiers.shift {
                        this.anchor = index;
                    }
                    if event.click_count >= 2 {
                        this.anchor = 0;
                        this.cursor = this.value.len();
                    }
                    this.selecting = true;
                    cx.notify();
                }),
            )
            .on_mouse_move(cx.listener(|this, event: &MouseMoveEvent, _, cx| {
                if this.selecting {
                    this.cursor = this.index_at(event.position);
                    cx.notify();
                }
            }))
            .on_mouse_up(
                MouseButton::Left,
                cx.listener(|this, _, _, _| this.selecting = false),
            )
            .on_mouse_up_out(
                MouseButton::Left,
                cx.listener(|this, _, _, _| this.selecting = false),
            )
            .child(paint::text_canvas(cx.entity()))
    }
}
