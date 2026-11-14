use crate::{Button, ButtonStyle, Icon, Theme, Tone, UI_FONT, column, row};
use gpui::{
    App, AppContext, Context, EventEmitter, FocusHandle, Focusable, KeyDownEvent, MouseButton,
    PromptButton, PromptHandle, PromptLevel, PromptResponse, RenderablePromptHandle, Role,
    SharedString, Window, prelude::*, px,
};

const PROMPT_WIDTH: f32 = 480.;
const PROMPT_MARGIN: f32 = 24.;
const PROMPT_PADDING: f32 = 28.;
const PROMPT_SECTION_GAP: f32 = 24.;
const PROMPT_TITLE_SIZE: f32 = 24.;
const PROMPT_TITLE_HEIGHT: f32 = 34.;
const PROMPT_DETAIL_SIZE: f32 = 16.;
const PROMPT_DETAIL_HEIGHT: f32 = 26.;
const PROMPT_BUTTON_GAP: f32 = 12.;

pub fn render_prompt(
    level: PromptLevel,
    message: &str,
    detail: Option<&str>,
    actions: &[PromptButton],
    handle: PromptHandle,
    window: &mut Window,
    cx: &mut App,
) -> RenderablePromptHandle {
    let view = cx.new(|cx| PromptDialog {
        level,
        message: message.to_owned().into(),
        detail: detail.map(|text| text.to_owned().into()),
        actions: actions
            .iter()
            .map(|button| (button.clone(), cx.focus_handle()))
            .collect(),
        scope: cx.focus_handle(),
        dismiss_focus: cx.focus_handle(),
    });
    // GPUI owns the response channel and restores the previous focus, including
    // when a confirmation is opened above an existing application dialog.
    handle.with_view(view, window, cx)
}

struct PromptDialog {
    level: PromptLevel,
    message: SharedString,
    detail: Option<SharedString>,
    actions: Vec<(PromptButton, FocusHandle)>,
    scope: FocusHandle,
    dismiss_focus: FocusHandle,
}

impl PromptDialog {
    fn cancel_index(&self) -> Option<usize> {
        self.actions
            .iter()
            .position(|(button, _)| button.is_cancel())
    }

    fn dismiss(&self, cx: &mut Context<Self>) {
        if let Some(index) = self.cancel_index() {
            cx.emit(PromptResponse(index));
        }
        cx.stop_propagation();
    }

    fn handle_key(&mut self, event: &KeyDownEvent, window: &mut Window, cx: &mut Context<Self>) {
        match event.keystroke.key.as_str() {
            "tab" => self.cycle_focus(event.keystroke.modifiers.shift, window, cx),
            "escape" => self.dismiss(cx),
            _ => {}
        }
        // A confirmation must not pass page shortcuts through to the obscured view.
        cx.stop_propagation();
    }

    fn cycle_focus(&self, backwards: bool, window: &mut Window, cx: &mut Context<Self>) {
        // GPUI lays prompts out separately from the root view's tab order. Own
        // this short cycle so traversal cannot get stuck or enter the background.
        let order: Vec<_> = self
            .cancel_index()
            .map(|_| &self.dismiss_focus)
            .into_iter()
            .chain(self.actions.iter().map(|(_, focus)| focus))
            .collect();
        if order.is_empty() {
            return;
        }
        let current = order.iter().position(|focus| focus.is_focused(window));
        let next = match (current, backwards) {
            (Some(0) | None, true) => order.len() - 1,
            (Some(index), true) => index - 1,
            (Some(index), false) => (index + 1) % order.len(),
            (None, false) => 0,
        };
        window.focus(order[next], cx);
    }

    fn button_style(&self, button: &PromptButton) -> ButtonStyle {
        match (self.level, button.is_cancel()) {
            (PromptLevel::Info, false) | (PromptLevel::Warning | PromptLevel::Critical, true) => {
                ButtonStyle::Primary
            }
            (PromptLevel::Info, true) => ButtonStyle::Secondary,
            (PromptLevel::Warning | PromptLevel::Critical, false) => {
                ButtonStyle::Tinted(Tone::Rose)
            }
        }
    }
}

impl Render for PromptDialog {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = Theme::current(window, cx);
        let heading = row()
            .items_start()
            .gap(px(PROMPT_BUTTON_GAP))
            .child(
                theme
                    .heading(self.message.clone(), PROMPT_TITLE_SIZE)
                    .line_height(px(PROMPT_TITLE_HEIGHT))
                    .flex_1()
                    .min_w_0(),
            )
            .when_some(self.cancel_index(), |row, index| {
                row.child(
                    Button::new("prompt-close", "")
                        .icon(Icon::Close)
                        .focus_handle(&self.dismiss_focus)
                        .compact()
                        .style(ButtonStyle::Ghost)
                        .tooltip(self.actions[index].0.label().clone())
                        .on_press(cx.listener(|this, (), _, cx| this.dismiss(cx))),
                )
            });
        let actions = row()
            .justify_end()
            .flex_wrap()
            .gap(px(PROMPT_BUTTON_GAP))
            .children(
                self.actions
                    .iter()
                    .enumerate()
                    .map(|(index, (button, focus))| {
                        Button::new(("prompt-action", index), button.label().clone())
                            .style(self.button_style(button))
                            .focus_handle(focus)
                            .on_press(
                                cx.listener(move |_, (), _, cx| cx.emit(PromptResponse(index))),
                            )
                    }),
            );
        let card = theme
            .card()
            .id("prompt-dialog")
            .role(Role::Dialog)
            .aria_label(self.message.clone())
            .when_some(self.detail.clone(), |card, detail| {
                card.aria_description(detail)
            })
            .w(px(PROMPT_WIDTH))
            .max_w(window.viewport_size().width - px(PROMPT_MARGIN * 2.))
            .max_h(window.viewport_size().height - px(PROMPT_MARGIN * 2.))
            .p(px(PROMPT_PADDING))
            .gap(px(PROMPT_SECTION_GAP))
            .overflow_y_scroll()
            .shadow_sm()
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .child(heading)
            .when_some(self.detail.clone(), |card, detail| {
                card.child(
                    theme
                        .label(detail, PROMPT_DETAIL_SIZE)
                        .text_color(theme.muted_text())
                        .line_height(px(PROMPT_DETAIL_HEIGHT)),
                )
            })
            .child(actions);
        column()
            .id("prompt-backdrop")
            .size_full()
            .items_center()
            .justify_center()
            .bg(theme.overlay())
            .font_family(UI_FONT)
            .text_color(theme.text())
            .occlude()
            .track_focus(&self.scope)
            .tab_group()
            .on_key_down(cx.listener(Self::handle_key))
            .on_mouse_down(
                MouseButton::Left,
                cx.listener(|this, _, _, cx| this.dismiss(cx)),
            )
            .on_scroll(|_, _, cx| cx.stop_propagation())
            .child(card)
    }
}

impl EventEmitter<PromptResponse> for PromptDialog {}

impl Focusable for PromptDialog {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        // Enter must preserve ongoing work unless the user deliberately focuses
        // another choice; source ordering is not an implicit destructive default.
        self.cancel_index()
            .and_then(|index| self.actions.get(index))
            .or_else(|| self.actions.first())
            .map_or_else(|| self.scope.clone(), |(_, focus)| focus.clone())
    }
}
