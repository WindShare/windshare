use crate::localization::{LanguageSettings, Text, language};
use crate::{
    flow_state::{CopyFeedback, ReceiveFlow, ShareFlow},
    layout::{FlowLayout, MAX_PAGE_WIDTH, page_gutter},
    model::{Filter, Overlay, Page, PreviewAction, Share, Transfer, demo_shares, demo_transfers},
    share_timing::lerp,
};
use gpui::{
    Animation, AnimationExt, App, AppContext, ElementId, Entity, FocusHandle, Focusable,
    KeyDownEvent, Subscription, Window, ease_out_quint, prelude::*, px,
};
use std::time::Duration;
use windshare_ui::InputEvent;
use windshare_ui::{
    Button, ButtonStyle, COMPACT_WIDTH, Icon, Input, Theme, Tone, UI_FONT, advance_focus, column,
    row,
};

pub struct Desktop {
    pub page: Page,
    pub filter: Filter,
    pub shares: Vec<Share>,
    pub transfers: Vec<Transfer>,
    pub grid: bool,
    pub search: Entity<Input>,
    pub link_input: Entity<Input>,
    pub share_flow: ShareFlow,
    pub preview_share_delay: Duration,
    pub motion_probe: crate::motion_probe::MotionProbe,
    pub receive_flow: ReceiveFlow,
    pub copy_feedback: CopyFeedback,
    pub primary_focus: FocusHandle,
    pub operation_sequence: usize,
    pub overlay: Option<Overlay>,
    pub toast: Option<(usize, Text)>,
    pub settings: [bool; 3],
    pub dialog_sequence: usize,
    toast_sequence: usize,
    pub focus: FocusHandle,
    pub modal_focus: FocusHandle,
    pub return_focus: Option<FocusHandle>,
    _subscriptions: Vec<Subscription>,
}
impl Desktop {
    pub fn new(window: &mut Window, preview_share_delay: Duration, cx: &mut Context<Self>) -> Self {
        let locale = language(cx);
        let search = cx.new(|cx| {
            Input::new(locale.text(Text::SearchShares), cx)
                .placeholder(locale.text(Text::SearchShares))
        });
        let link_input =
            cx.new(|cx| Input::new(locale.text(Text::ShareLink), cx).accent(Tone::Lavender));
        let subscriptions = vec![
            cx.observe_global_in::<LanguageSettings>(window, |this, window, cx| {
                let locale = language(cx);
                window.set_window_title(locale.text(Text::WindowTitle));
                this.search.update(cx, |input, cx| {
                    input.set_labels(
                        locale.text(Text::SearchShares),
                        locale.text(Text::SearchShares),
                        cx,
                    );
                });
                this.link_input.update(cx, |input, cx| {
                    input.set_labels(locale.text(Text::ShareLink), "", cx);
                });
                cx.notify();
            }),
            windshare_ui::follow_system_theme(window),
            cx.subscribe(&search, |_, _, _, cx| cx.notify()),
            cx.subscribe_in(&link_input, window, |this, _, event, window, cx| {
                if matches!(event, InputEvent::Submit) {
                    this.dispatch(PreviewAction::OpenPreview, window, cx);
                } else {
                    if matches!(this.receive_flow, ReceiveFlow::Link { .. }) {
                        this.receive_flow = ReceiveFlow::default();
                    }
                    cx.notify();
                }
            }),
        ];
        let focus = cx.focus_handle();
        let primary_focus = cx.focus_handle();
        window.focus(&primary_focus, cx);
        Self {
            page: Page::Share,
            filter: Filter::All,
            shares: demo_shares(),
            transfers: demo_transfers(),
            grid: true,
            search,
            link_input,
            share_flow: ShareFlow::Choose,
            preview_share_delay,
            motion_probe: crate::motion_probe::MotionProbe::default(),
            receive_flow: ReceiveFlow::default(),
            copy_feedback: CopyFeedback::Idle,
            primary_focus,
            operation_sequence: 0,
            overlay: None,
            toast: None,
            settings: [true, true, false],
            dialog_sequence: 0,
            toast_sequence: 0,
            focus,
            modal_focus: cx.focus_handle(),
            return_focus: None,
            _subscriptions: subscriptions,
        }
    }
    pub fn button(
        id: impl Into<ElementId>,
        text: impl Into<gpui::SharedString>,
        symbol: Icon,
        style: ButtonStyle,
        action: PreviewAction,
        cx: &Context<Self>,
    ) -> Button {
        Button::new(id, text)
            .icon(symbol)
            .style(style)
            .on_press(cx.listener(move |this, (), window, cx| this.dispatch(action, window, cx)))
    }
    pub fn notify(&mut self, message: Text, cx: &mut Context<Self>) {
        if !self.settings[0] {
            return;
        }
        self.toast_sequence += 1;
        let id = self.toast_sequence;
        self.toast = Some((id, message));
        cx.spawn(async move |view, cx| {
            cx.background_executor().timer(Duration::from_secs(4)).await;
            let _ = view.update(cx, |this, cx| {
                if this
                    .toast
                    .as_ref()
                    .is_some_and(|(current, _)| *current == id)
                {
                    this.toast = None;
                    cx.notify();
                }
            });
        })
        .detach();
        cx.notify();
    }
    pub fn open_overlay(&mut self, overlay: Overlay, window: &mut Window, cx: &mut Context<Self>) {
        if self.overlay.is_none() {
            self.return_focus = window.focused(cx);
        }
        self.overlay = Some(overlay);
        self.dialog_sequence += 1;
        window.focus(&self.modal_focus, cx);
    }
    pub fn close_overlay(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.overlay = None;
        window.focus(self.return_focus.as_ref().unwrap_or(&self.focus), cx);
        self.return_focus = None;
    }
    pub fn navigate(&mut self, page: Page, window: &mut Window, cx: &mut Context<Self>) {
        self.page = page;
        self.overlay = None;
        self.return_focus = None;
        self.focus_task(window, cx);
    }
    pub fn focus_task(&self, window: &mut Window, cx: &mut App) {
        let focus = match self.page {
            Page::Receive if matches!(self.receive_flow, ReceiveFlow::Link { .. }) => {
                self.link_input.read(cx).focus_handle(cx)
            }
            Page::Share | Page::Receive => self.primary_focus.clone(),
            _ => self.focus.clone(),
        };
        window.focus(&focus, cx);
    }
    fn handle_key(&mut self, event: &KeyDownEvent, window: &mut Window, cx: &mut Context<Self>) {
        let key = &event.keystroke;
        if key.key == "tab" {
            let scope = if self.overlay.is_some() {
                &self.modal_focus
            } else {
                &self.focus
            };
            advance_focus(scope, key.modifiers.shift, window, cx);
            cx.stop_propagation();
        } else if key.key == "escape" && self.overlay.is_some() {
            self.close_overlay(window, cx);
            cx.notify();
        } else if key.modifiers.control && key.key == "n" {
            self.dispatch(PreviewAction::NewShare, window, cx);
        } else if key.modifiers.control && key.key == "k" && self.overlay.is_none() {
            self.navigate(Page::Shares, window, cx);
            let focus = self.search.read(cx).focus_handle(cx);
            window.focus(&focus, cx);
            cx.notify();
        }
    }
    pub const fn reduced_motion(&self) -> bool {
        self.settings[2]
    }
}
impl Render for Desktop {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let locale = language(cx);
        let theme = Theme::from(window.appearance());
        let compact = window.viewport_size().width < px(COMPACT_WIDTH);
        let layout = FlowLayout::for_window(window);
        let content = match self.page {
            Page::Share => self.sharing_page(layout, window, cx),
            Page::Receive => self.receiving_page(theme, layout, cx),
            Page::Shares => self.shares_page(theme, compact, cx),
            Page::Transfers => self.transfers_page(theme, cx),
            Page::Settings => self.settings_page(theme, cx),
            Page::Components => Self::components_page(theme, cx),
        };
        column()
            .size_full()
            .relative()
            .bg(theme.canvas())
            .font_family(UI_FONT)
            .text_color(theme.text())
            .text_size(px(16.))
            .track_focus(&self.focus)
            .tab_group()
            .on_key_down(cx.listener(Self::handle_key))
            .child(
                row()
                    .items_stretch()
                    .flex_1()
                    .min_h_0()
                    .child(self.sidebar(theme, compact, cx))
                    .child(
                        column()
                            .flex_1()
                            .min_w_0()
                            .child(Self::titlebar(theme, cx))
                            .child(
                                column()
                                    .id(("page-scroll", self.page as usize))
                                    .overflow_y_scroll()
                                    .items_center()
                                    .flex_1()
                                    .min_h_0()
                                    .px(px(page_gutter(compact)))
                                    .pt(px(layout.page_top()))
                                    .pb(px(32.))
                                    .child(
                                        column()
                                            .w_full()
                                            .max_w(px(MAX_PAGE_WIDTH))
                                            .gap(px(24.))
                                            .child(self.page_heading(theme, compact, cx))
                                            .child(
                                                content.with_animation(
                                                    ("page-enter", self.page as usize),
                                                    Animation::new(Duration::from_millis(
                                                        if self.reduced_motion() { 1 } else { 260 },
                                                    ))
                                                    .with_easing(ease_out_quint()),
                                                    gpui::Styled::opacity,
                                                ),
                                            ),
                                    ),
                            )
                            .child(Self::statusbar(theme, cx)),
                    ),
            )
            .when(self.overlay.is_some(), |root| {
                root.child(self.overlay_view(window, cx))
            })
            .when_some(self.toast, |root, (id, text)| {
                root.child(
                    row()
                        .absolute()
                        .bottom(px(52.))
                        .right(px(32.))
                        .gap(px(12.))
                        .px(px(22.))
                        .py(px(16.))
                        .rounded(px(16.))
                        .bg(theme.notification_surface())
                        .border_1()
                        .border_color(theme.notification_border())
                        .shadow_sm()
                        .child(theme.icon_tile(Icon::Check, Tone::Mint, 30.))
                        .child(
                            theme
                                .label(locale.text(text), 16.)
                                .text_color(theme.accent_text()),
                        )
                        .with_animation(
                            ("toast", id),
                            Animation::new(Duration::from_millis(if self.reduced_motion() {
                                1
                            } else {
                                280
                            }))
                            .with_easing(ease_out_quint()),
                            |el, delta| el.opacity(delta).bottom(px(lerp(44., 52., delta))),
                        ),
                )
            })
    }
}
