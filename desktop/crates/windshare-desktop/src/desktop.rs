use crate::localization::{LanguageSettings, Text, language};
use crate::{
    flow_state::{CopyFeedback, ReceiveFlow, ShareFlow},
    layout::{FlowLayout, MAX_PAGE_WIDTH, page_gutter},
    live::Runtime,
    model::{Action, Filter, Overlay, Page, Share, Transfer, demo_shares, demo_transfers},
    overlays::OverlayFrame,
    share_timing::lerp,
    sharing::LinkDisclosure,
};
use gpui::{
    Animation, AnimationExt, App, AppContext, ElementId, Entity, FocusHandle, Focusable,
    KeyDownEvent, Subscription, Window, ease_out_quint, prelude::*, px,
};
use std::time::Duration;
use windshare_ui::InputEvent;
use windshare_ui::{
    Button, ButtonStyle, COMPACT_WIDTH, Icon, Input, Theme, Tone, UI_FONT, advance_focus, column,
    icon, row,
};

pub struct Desktop {
    pub page: Page,
    pub super_share: Entity<crate::supershare::SuperShare>,
    pub filter: Filter,
    pub shares: Vec<Share>,
    pub transfers: Vec<Transfer>,
    pub grid: bool,
    pub search: Entity<Input>,
    pub link_input: Entity<Input>,
    pub share_flow: ShareFlow,
    pub runtime: Runtime,
    pub setup_form: crate::live::SetupForm,
    pub motion_probe: crate::motion_probe::MotionProbe,
    pub receive_flow: ReceiveFlow,
    pub copy_feedback: CopyFeedback,
    pub primary_focus: FocusHandle,
    pub operation_sequence: usize,
    pub overlays: Vec<OverlayFrame>,
    pub link_disclosure: LinkDisclosure,
    pub toast: Option<(usize, Text)>,
    pub settings: [bool; 3],
    pub dialog_sequence: usize,
    toast_sequence: usize,
    pub focus: FocusHandle,
    _subscriptions: Vec<Subscription>,
}
impl Desktop {
    pub fn new(window: &mut Window, runtime: Runtime, cx: &mut Context<Self>) -> Self {
        let preview = runtime.is_preview();
        let locale = language(cx);
        let search = cx.new(|cx| {
            Input::new(locale.text(Text::SearchShares), cx)
                .placeholder(locale.text(Text::SearchShares))
        });
        let link_input =
            cx.new(|cx| Input::new(locale.text(Text::ShareLink), cx).accent(Tone::Lavender));
        let super_share = cx.new(|cx| crate::supershare::SuperShare::new(window, cx));
        let setup_form = crate::live::SetupForm::new(cx);
        let subscriptions = vec![
            cx.observe(&super_share, |_, _, cx| cx.notify()),
            cx.subscribe_in(&super_share, window, |this, _, _, window, cx| {
                this.navigate(Page::Share, window, cx);
                cx.notify();
            }),
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
                this.setup_form.localize(cx);
                cx.notify();
            }),
            windshare_ui::follow_system_theme(window),
            cx.subscribe(&search, |_, _, _, cx| cx.notify()),
            cx.subscribe_in(&link_input, window, |this, _, event, window, cx| {
                if matches!(event, InputEvent::Submit) {
                    this.dispatch(Action::OpenPreview, window, cx);
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
        if let Runtime::Native(session) = &runtime {
            Self::observe_runtime(session.controller.clone(), window, cx);
        }
        Self {
            page: Page::Share,
            super_share,
            filter: Filter::All,
            shares: if preview { demo_shares() } else { Vec::new() },
            transfers: if preview {
                demo_transfers()
            } else {
                Vec::new()
            },
            grid: true,
            search,
            link_input,
            share_flow: ShareFlow::Choose,
            runtime,
            setup_form,
            motion_probe: crate::motion_probe::MotionProbe::default(),
            receive_flow: ReceiveFlow::default(),
            copy_feedback: CopyFeedback::Idle,
            primary_focus,
            operation_sequence: if preview { 3 } else { 0 },
            overlays: Vec::new(),
            link_disclosure: LinkDisclosure::default(),
            toast: None,
            settings: [true, true, false],
            dialog_sequence: 0,
            toast_sequence: 0,
            focus,
            _subscriptions: subscriptions,
        }
    }
    pub fn button(
        id: impl Into<ElementId>,
        text: impl Into<gpui::SharedString>,
        symbol: Icon,
        style: ButtonStyle,
        action: Action,
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
        self.dialog_sequence += 1;
        let scope = cx.focus_handle();
        self.overlays.push(OverlayFrame {
            content: overlay,
            return_focus: window.focused(cx),
            scope: scope.clone(),
            sequence: self.dialog_sequence,
        });
        window.focus(&scope, cx);
    }
    pub fn close_overlay(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(frame) = self.overlays.pop() else {
            return;
        };
        let fallback = self
            .overlays
            .last()
            .map_or(&self.focus, |frame| &frame.scope);
        window.focus(frame.return_focus.as_ref().unwrap_or(fallback), cx);
        tracing::info!(
            event = "desktop.overlay_closed",
            session = std::process::id(),
            overlay = ?frame.content,
            remaining = self.overlays.len(),
        );
    }
    pub fn navigate(&mut self, page: Page, window: &mut Window, cx: &mut Context<Self>) {
        if !self.page_available(page) {
            return;
        }
        if page == Page::SuperShare {
            self.super_share
                .update(cx, crate::supershare::SuperShare::enter);
        } else if self.page == Page::SuperShare {
            self.super_share
                .update(cx, crate::supershare::SuperShare::leave);
        }
        self.page = page;
        self.overlays.clear();
        self.link_disclosure = LinkDisclosure::Summary;
        self.focus_task(window, cx);
    }
    pub fn focus_task(&self, window: &mut Window, cx: &mut App) {
        let focus = match self.page {
            Page::Receive if matches!(self.receive_flow, ReceiveFlow::Link { .. }) => {
                self.link_input.read(cx).focus_handle(cx)
            }
            Page::Share | Page::Receive => self.primary_focus.clone(),
            Page::SuperShare => self.super_share.read(cx).focus_handle(cx),
            _ => self.focus.clone(),
        };
        window.focus(&focus, cx);
    }
    fn handle_key(&mut self, event: &KeyDownEvent, window: &mut Window, cx: &mut Context<Self>) {
        let key = &event.keystroke;
        if key.key == "tab" {
            let scope = self
                .overlays
                .last()
                .map_or(&self.focus, |frame| &frame.scope);
            advance_focus(scope, key.modifiers.shift, window, cx);
            cx.stop_propagation();
        } else if key.key == "escape" && !self.overlays.is_empty() {
            self.close_overlay(window, cx);
            cx.notify();
        } else if key.modifiers.control && key.key == "n" {
            self.dispatch(Action::NewShare, window, cx);
        } else if key.modifiers.control && key.key == "k" && self.overlays.is_empty() {
            self.navigate(Page::Shares, window, cx);
            let focus = self.search.read(cx).focus_handle(cx);
            window.focus(&focus, cx);
            cx.notify();
        }
    }
    fn page_content(
        &mut self,
        layout: FlowLayout,
        window: &Window,
        cx: &Context<Self>,
    ) -> gpui::Div {
        let theme = Theme::from(window.appearance());
        let compact = window.viewport_size().width < px(COMPACT_WIDTH);
        match self.page {
            Page::Share => column().child(self.sharing_page(layout, window, cx)),
            Page::Receive => self.receiving_page(theme, layout, cx),
            Page::Shares => self.shares_page(theme, compact, cx),
            Page::Transfers => self.transfers_page(theme, cx),
            Page::Settings => self.settings_page(theme, cx),
            Page::Components => Self::components_page(theme, cx),
            Page::SuperShare => column().child(self.super_share.clone()),
        }
    }

    fn main_pane(&mut self, window: &Window, cx: &Context<Self>) -> gpui::Div {
        let theme = Theme::from(window.appearance());
        // A showcase owns its layout; workspace gutters must not crop its visual canvas.
        let body = if self.page == Page::SuperShare {
            column().flex_1().min_h_0().child(self.super_share.clone())
        } else {
            self.workspace_page(window, cx)
        };
        column()
            .flex_1()
            .min_w_0()
            .child(Self::titlebar(window, cx))
            .child(body)
            .child(self.statusbar(theme, cx))
    }

    fn workspace_page(&mut self, window: &Window, cx: &Context<Self>) -> gpui::Div {
        let theme = Theme::from(window.appearance());
        let compact = window.viewport_size().width < px(COMPACT_WIDTH);
        let layout = FlowLayout::for_window(window);
        let content = self.page_content(layout, window, cx);
        column().flex_1().min_h_0().child(
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
                        // Share scenes own their headings and occupy the workspace.
                        // A persistent page introduction competes with the result.
                        .when(self.page == Page::Share, |el| el.flex_1().justify_center())
                        .when(self.page != Page::Share, |el| {
                            el.child(self.page_heading(theme, compact, cx))
                        })
                        .when_some(self.runtime.notice(), |el, error| {
                            el.child(
                                theme.card().p(px(18.)).child(
                                    theme.label(error, 15.).text_color(Tone::Rose.text(theme)),
                                ),
                            )
                        })
                        .child(
                            content.with_animation(
                                ("page-enter", self.page as usize),
                                Animation::new(Duration::from_millis(if self.reduced_motion() {
                                    1
                                } else {
                                    260
                                }))
                                .with_easing(ease_out_quint()),
                                gpui::Styled::opacity,
                            ),
                        ),
                ),
        )
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
        let pane = self.main_pane(window, cx);
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
            .on_drop(
                cx.listener(|this, paths: &gpui::ExternalPaths, window, cx| {
                    cx.stop_propagation();
                    this.create_native_share(paths.paths().to_vec(), Icon::Folder, window, cx);
                }),
            )
            .when(self.page == Page::SuperShare, |root| {
                root.child(crate::supershare::backdrop(theme))
            })
            .child(
                row()
                    .items_stretch()
                    .flex_1()
                    .min_h_0()
                    .child(self.sidebar(theme, compact, cx))
                    .child(pane),
            )
            .when(!self.overlays.is_empty(), |root| {
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
                        .child(icon(Icon::Check, 18.).text_color(theme.accent_text()))
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
