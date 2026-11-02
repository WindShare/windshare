use super::*;

impl EntityInputHandler for Input {
    fn text_for_range(
        &mut self,
        range: Range<usize>,
        actual: &mut Option<Range<usize>>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<String> {
        let bytes = self.byte_range(range);
        *actual = Some(self.utf16_range(bytes.clone()));
        Some(self.value[bytes].to_owned())
    }
    fn selected_text_range(
        &mut self,
        _: bool,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<UTF16Selection> {
        Some(UTF16Selection {
            range: self.utf16_range(self.selection()),
            reversed: self.cursor < self.anchor,
        })
    }
    fn set_selected_text_range(
        &mut self,
        range: Range<usize>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let bytes = self.byte_range(range);
        self.anchor = bytes.start;
        self.cursor = bytes.end;
        window.notify_text_selection_changed(&self.focus);
        cx.notify();
    }
    fn text_length_utf16(&mut self, _: &mut Window, _: &mut Context<Self>) -> Option<usize> {
        Some(self.value.encode_utf16().count())
    }
    fn marked_text_range(&self, _: &mut Window, _: &mut Context<Self>) -> Option<Range<usize>> {
        self.marked.clone().map(|r| self.utf16_range(r))
    }
    fn unmark_text(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.marked = None;
        window.notify_text_input_changed(&self.focus);
        cx.notify();
    }
    fn replace_text_in_range(
        &mut self,
        range: Option<Range<usize>>,
        text: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let bytes = range
            .map(|r| self.byte_range(r))
            .or(self.marked.clone())
            .unwrap_or(self.selection());
        self.replace(bytes, text, window, cx);
    }
    fn replace_and_mark_text_in_range(
        &mut self,
        range: Option<Range<usize>>,
        text: &str,
        selection: Option<Range<usize>>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let bytes = range
            .map(|r| self.byte_range(r))
            .or(self.marked.clone())
            .unwrap_or(self.selection());
        let start = bytes.start;
        self.replace(bytes, text, window, cx);
        self.marked = (!text.is_empty()).then_some(start..self.cursor);
        if let Some(selection) = selection {
            let origin = self.to_utf16(start);
            let units = text.encode_utf16().count();
            self.anchor = self.byte_offset(origin + selection.start.min(units));
            self.cursor = self.byte_offset(origin + selection.end.min(units));
        }
        window.notify_text_input_changed(&self.focus);
    }
    fn bounds_for_range(
        &mut self,
        range: Range<usize>,
        bounds: Bounds<Pixels>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<Bounds<Pixels>> {
        let bytes = self.byte_range(range);
        let line = self.layout.as_ref()?;
        Some(Bounds::from_corners(
            point(
                bounds.left() + line.x_for_index(bytes.start) - self.scroll,
                bounds.top(),
            ),
            point(
                bounds.left() + line.x_for_index(bytes.end) - self.scroll,
                bounds.bottom(),
            ),
        ))
    }
    fn character_index_for_point(
        &mut self,
        point: Point<Pixels>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<usize> {
        Some(self.to_utf16(self.index_at(point)))
    }
}
