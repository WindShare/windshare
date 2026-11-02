use crate::{flow_state::*, layout::FlowLayout, model::*};
use gpui::{prelude::*, *};
use std::time::Duration;
use windshare_ui::InputEvent;
use windshare_ui::*;

pub struct Desktop {
    pub page: Page,
    pub filter: Filter,
    pub shares: Vec<Share>,
    pub transfers: Vec<Transfer>,
    pub grid: bool,
    pub search: Entity<Input>,
    pub link_input: Entity<Input>,
    pub share_flow: ShareFlow,
    pub receive_flow: ReceiveFlow,
    pub copy_feedback: CopyFeedback,
    pub primary_focus: FocusHandle,
    pub operation_sequence: usize,
    pub overlay: Option<Overlay>,
    pub toast: Option<(usize, SharedString)>,
    pub settings: [bool; 3],
    pub dialog_sequence: usize,
    toast_sequence: usize,
    pub focus: FocusHandle,
    pub modal_focus: FocusHandle,
    pub return_focus: Option<FocusHandle>,
    _subscriptions: Vec<Subscription>,
}
impl Desktop {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let search = cx.new(|cx| Input::new("搜索分享名称", cx));
        let link_input = cx.new(|cx| Input::new("粘贴 WindShare 分享链接", cx));
        let subscriptions = vec![
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
        &self,
        id: impl Into<ElementId>,
        text: impl Into<SharedString>,
        symbol: Icon,
        style: ButtonStyle,
        action: PreviewAction,
        cx: &Context<Self>,
    ) -> Button {
        Button::new(id, text).icon(symbol).style(style).on_press(
            cx.listener(move |this, _, window, cx| this.dispatch(action.clone(), window, cx)),
        )
    }
    pub fn notify(&mut self, message: impl Into<SharedString>, cx: &mut Context<Self>) {
        if !self.settings[0] {
            return;
        }
        self.toast_sequence += 1;
        let id = self.toast_sequence;
        self.toast = Some((id, message.into()));
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
            Page::Share => self.primary_focus.clone(),
            Page::Receive if matches!(self.receive_flow, ReceiveFlow::Link { .. }) => {
                self.link_input.read(cx).focus_handle(cx)
            }
            Page::Receive => self.primary_focus.clone(),
            _ => self.focus.clone(),
        };
        window.focus(&focus, cx);
    }
    pub fn reduced_motion(&self) -> bool {
        self.settings[2]
    }
}
impl Render for Desktop {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let compact = window.viewport_size().width < px(COMPACT_WIDTH);
        let layout = FlowLayout::for_window(window);
        let content = match self.page {
            Page::Share => self.sharing_page(layout, cx),
            Page::Receive => self.receiving_page(layout, cx),
            Page::Shares => self.shares_page(compact, cx),
            Page::Transfers => self.transfers_page(cx),
            Page::Settings => self.settings_page(cx),
            Page::Components => self.components_page(cx),
        };
        column()
            .size_full()
            .relative()
            .bg(canvas_color())
            .font_family(UI_FONT)
            .text_color(ink())
            .text_size(px(16.))
            .track_focus(&self.focus)
            .tab_group()
            .on_key_down(cx.listener(|this, event: &KeyDownEvent, window, cx| {
                let key = &event.keystroke;
                if key.key == "tab" {
                    let scope = if this.overlay.is_some() {
                        &this.modal_focus
                    } else {
                        &this.focus
                    };
                    advance_focus(scope, key.modifiers.shift, window, cx);
                    cx.stop_propagation();
                } else if key.key == "escape" && this.overlay.is_some() {
                    this.close_overlay(window, cx);
                    cx.notify();
                } else if key.modifiers.control && key.key == "n" {
                    this.dispatch(PreviewAction::NewShare, window, cx);
                } else if key.modifiers.control && key.key == "k" && this.overlay.is_none() {
                    this.navigate(Page::Shares, window, cx);
                    let focus = this.search.read(cx).focus_handle(cx);
                    window.focus(&focus, cx);
                    cx.notify();
                }
            }))
            .child(self.titlebar(cx))
            .child(
                row()
                    .items_stretch()
                    .flex_1()
                    .min_h_0()
                    .child(self.sidebar(compact, cx))
                    .child(
                        column()
                            .flex_1()
                            .min_w_0()
                            .child(
                                column()
                                    .id(("page-scroll", self.page as usize))
                                    .overflow_y_scroll()
                                    .items_center()
                                    .flex_1()
                                    .min_h_0()
                                    .px(px(if compact { 28. } else { 48. }))
                                    .pt(px(layout.page_top()))
                                    .pb(px(32.))
                                    .child(
                                        column()
                                            .w_full()
                                            .max_w(px(960.))
                                            .gap(px(24.))
                                            .child(self.page_heading(compact, cx))
                                            .child(
                                                content.with_animation(
                                                    ("page-enter", self.page as usize),
                                                    Animation::new(Duration::from_millis(
                                                        if self.reduced_motion() { 1 } else { 260 },
                                                    ))
                                                    .with_easing(ease_out_quint()),
                                                    |el, delta| el.opacity(delta),
                                                ),
                                            ),
                                    ),
                            )
                            .child(self.statusbar(cx)),
                    ),
            )
            .when(self.overlay.is_some(), |root| {
                root.child(self.overlay_view(window, cx))
            })
            .when_some(self.toast.clone(), |root, (id, text)| {
                root.child(
                    row()
                        .absolute()
                        .bottom(px(52.))
                        .right(px(32.))
                        .gap(px(12.))
                        .px(px(22.))
                        .py(px(16.))
                        .rounded(px(16.))
                        .bg(notification_surface())
                        .border_1()
                        .border_color(notification_border())
                        .shadow_sm()
                        .child(icon_tile(Icon::Check, Tone::Mint, 30.).bg(action_surface()))
                        .child(label(text, 16.).text_color(primary()))
                        .with_animation(
                            ("toast", id),
                            Animation::new(Duration::from_millis(if self.reduced_motion() {
                                1
                            } else {
                                280
                            }))
                            .with_easing(ease_out_quint()),
                            |el, delta| el.opacity(delta).bottom(px(44. + 8. * delta)),
                        ),
                )
            })
    }
}
