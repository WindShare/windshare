use crate::localization::{LanguageSettings, Text, language};
mod behavior;
mod presentation;

use crate::artwork::Artwork;
use behavior::{Behavior, NOMINAL_SIZE, Pose, SMILE_DURATION};
use gpui::{
    App, Bounds, ElementId, FocusHandle, IntoElement, MouseButton, MouseMoveEvent, Pixels, Point,
    Radians, Role, Subscription, Task, Window, canvas, div, point, prelude::*, px,
};
use presentation::Presentation;
pub use presentation::SpiritState;
use std::time::Instant;
use windshare_ui::Theme;

#[derive(IntoElement)]
pub struct Spirit {
    id: ElementId,
    presentation: Presentation,
}

impl Spirit {
    pub fn new(
        id: impl Into<ElementId>,
        state: SpiritState,
        center: Point<f32>,
        width: f32,
        reduced_motion: bool,
    ) -> Self {
        Self {
            id: id.into(),
            presentation: Presentation {
                state,
                center,
                width,
                reduced_motion,
            },
        }
    }
}

impl RenderOnce for Spirit {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let view = window.use_keyed_state(self.id, cx, |window, cx| {
            SpiritView::new(self.presentation, window, cx)
        });
        view.update(cx, |view, _| view.presentation = self.presentation);
        view
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum MotionMode {
    Suspended,
    Reduced,
    Choreographed,
    Animated,
}

struct SpiritView {
    presentation: Presentation,
    behavior: Behavior,
    mode: MotionMode,
    last_frame: Instant,
    bounds: Option<Bounds<Pixels>>,
    pointer_position: Option<Point<Pixels>>,
    focus: FocusHandle,
    smile_wakeup: Option<Task<()>>,
    _subscriptions: Vec<Subscription>,
}

impl SpiritView {
    fn new(presentation: Presentation, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let subscriptions = vec![
            cx.observe_global::<LanguageSettings>(|_, cx| cx.notify()),
            cx.observe_window_activation(window, |this, _, cx| {
                this.behavior.reset();
                this.pointer_position = None;
                this.last_frame = Instant::now();
                cx.notify();
            }),
            cx.observe_window_bounds(window, |this, _, cx| {
                this.release("window_bounds_changed", cx);
                this.last_frame = Instant::now();
                cx.notify();
            }),
        ];
        Self {
            presentation,
            behavior: Behavior::default(),
            mode: MotionMode::Suspended,
            last_frame: Instant::now(),
            bounds: None,
            pointer_position: None,
            focus: cx.focus_handle(),
            smile_wakeup: None,
            _subscriptions: subscriptions,
        }
    }

    fn motion_mode(&self, window: &Window, cx: &App) -> MotionMode {
        if !window.is_window_active() || !window.is_visible() {
            MotionMode::Suspended
        } else if self.presentation.reduced_motion || cx.reduce_motion() {
            MotionMode::Reduced
        } else if matches!(self.presentation.state, SpiritState::Choreographed { .. }) {
            MotionMode::Choreographed
        } else {
            MotionMode::Animated
        }
    }

    fn release(&mut self, reason: &'static str, cx: &mut Context<Self>) {
        if self.behavior.release() {
            tracing::info!(
                event = "desktop.spirit_released",
                session = std::process::id(),
                spirit = ?cx.entity_id(),
                reason
            );
            cx.notify();
        }
    }

    fn follow(&mut self, event: &MouseMoveEvent, window: &mut Window, cx: &mut Context<Self>) {
        if self.motion_mode(window, cx) != MotionMode::Animated {
            return;
        }
        let Some(bounds) = self.bounds else { return };
        let previous_position = self.pointer_position.replace(event.position);
        if event.pressed_button.is_some() {
            self.release("pointer_pressed", cx);
            return;
        }
        // Native mouse-up and keyboard activation can replay a move at the
        // same position. Those notifications must not cancel the tap impulse.
        if previous_position == Some(event.position) {
            return;
        }
        let offset = event.position - bounds.center();
        let target = Pose::for_pointer(offset.x / bounds.size.width, offset.y / bounds.size.height);
        if !self.behavior.following() {
            tracing::info!(
                event = "desktop.spirit_follow_started",
                session = std::process::id(),
                spirit = ?cx.entity_id(),
                target_x = target.x,
                target_y = target.y
            );
        }
        self.behavior.follow(target, Instant::now());
        cx.notify();
    }

    fn tap(&mut self, event: &gpui::ClickEvent, window: &mut Window, cx: &mut Context<Self>) {
        let mode = self.motion_mode(window, cx);
        if mode == MotionMode::Suspended {
            return;
        }
        cx.stop_propagation();
        self.behavior
            .tap(Instant::now(), mode == MotionMode::Animated);
        tracing::info!(
            event = "desktop.spirit_tapped",
            session = std::process::id(),
            spirit = ?cx.entity_id(),
            input = ?event,
            ?mode
        );
        // A single cancelable deadline also clears smile feedback in reduced
        // motion mode, without starting a decorative frame loop.
        self.smile_wakeup = Some(cx.spawn(async move |view, cx| {
            cx.background_executor().timer(SMILE_DURATION).await;
            let _ = view.update(cx, |_, cx| cx.notify());
        }));
        cx.notify();
    }

    fn pose(&mut self, window: &Window, cx: &Context<Self>) -> Pose {
        let now = Instant::now();
        let mode = self.motion_mode(window, cx);
        if self.mode != mode {
            self.behavior.reset();
            self.last_frame = now;
            self.mode = mode;
            tracing::info!(
                event = "desktop.spirit_motion_changed",
                session = std::process::id(),
                spirit = ?cx.entity_id(),
                ?mode
            );
        }
        let elapsed = now.saturating_duration_since(self.last_frame);
        self.last_frame = now;
        if mode != MotionMode::Animated {
            return Pose::default();
        }
        let was_following = self.behavior.following();
        let mut pose = self.behavior.advance(elapsed, now);
        pose.y += self.behavior.float_offset();
        if was_following && !self.behavior.following() {
            tracing::info!(
                event = "desktop.spirit_released",
                session = std::process::id(),
                spirit = ?cx.entity_id(),
                reason = "pointer_paused"
            );
        }
        pose
    }
}

impl Render for SpiritView {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let pose = self.pose(window, cx);
        let presentation = self.presentation;
        let scale = presentation.width / NOMINAL_SIZE;
        let center = presentation.center + point(pose.x * scale, pose.y * scale);
        let smile = presentation.smile(self.behavior.smiling(Instant::now()));
        // Keep the artwork in scene coordinates for its entire lifetime. The
        // stationary focus/hit target is a sibling, so enabling it cannot change
        // rasterization, border insets, or the ambient motion.
        div()
            .absolute()
            .inset_0()
            .size_full()
            .child(
                Artwork::Spirit
                    .blend_to(Artwork::Happy, center, presentation.width, smile)
                    .rotate(Radians(pose.tilt.to_radians())),
            )
            .when_some(presentation.hit_center(), |el, anchor| {
                el.child(self.hit_target(anchor, window, cx))
            })
    }
}

impl SpiritView {
    fn hit_target(
        &self,
        center: Point<f32>,
        window: &Window,
        cx: &Context<Self>,
    ) -> impl IntoElement {
        let theme = Theme::current(window, cx);
        let width = self.presentation.width;
        let observer = cx.weak_entity();
        div()
            .id("spirit-target")
            .absolute()
            .left(px(center.x - width / 2.))
            .top(px(center.y - width / 2.))
            .size(px(width))
            .rounded_full()
            .border_1()
            .border_color(gpui::transparent_black())
            .role(Role::Button)
            .aria_label(language(cx).text(Text::SpiritGreeting))
            .track_focus(&self.focus)
            .tab_index(0)
            .cursor_pointer()
            .focus_visible(|style| style.border_color(theme.focus_ring()))
            .on_mouse_move(cx.listener(Self::follow))
            .on_mouse_down(
                MouseButton::Left,
                cx.listener(|this, _, _, cx| {
                    this.release("pointer_pressed", cx);
                }),
            )
            .hover_listener_mode(gpui::HoverListenerMode::InputModalityIndependent)
            .on_hover(cx.listener(|this, hovered, _, cx| {
                if !hovered {
                    this.pointer_position = None;
                    this.release("pointer_left", cx);
                }
            }))
            .on_mouse_exit(cx.listener(|this, _, _, cx| this.release("window_left", cx)))
            .on_scroll(cx.listener(|this, _, _, cx| this.release("scrolled", cx)))
            .on_click(cx.listener(Self::tap))
            .child(
                canvas(
                    move |bounds, window, cx| {
                        let visible = bounds.intersects(&window.content_mask().bounds);
                        let _ = observer.update(cx, |view, cx| {
                            // The hit target never follows the animated artwork. Layout
                            // changes release old coordinates before the next pointer event.
                            if view.bounds.is_some_and(|previous| previous != bounds) {
                                view.pointer_position = None;
                                view.release("layout_changed", cx);
                            }
                            view.bounds = Some(bounds);
                            if visible && view.motion_mode(window, cx) == MotionMode::Animated {
                                window.request_animation_frame();
                            } else if !visible {
                                view.behavior.reset();
                                view.last_frame = Instant::now();
                            }
                        });
                    },
                    |_, (), _, _| {},
                )
                .absolute()
                .inset_0()
                .size_full(),
            )
    }
}
