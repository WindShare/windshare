//! A stable QR frame with a whirl bloom or a plain fade when the link arrives.
mod pending;
mod state;

use pending::waiting_qr;
pub(super) use state::Arrival;
use state::{FADE, Phase, Signal};

use crate::motion::whirl::{
    self, Opening, Standing, Tones,
    bloom::{self, BLOOM},
    easing::smoothstep,
    seconds_between,
};
use gpui::{
    App, Bounds, BoxShadow, Context, ElementId, IntoElement, Pixels, SharedString, Subscription,
    Window, canvas, div, point, prelude::*, px,
};
use std::{cell::Cell, rc::Rc, time::Instant};
use windshare_ui::{Icon, QrCodeImage, Theme, column, icon};

pub(super) const QR_INLINE_SIDE: f32 = 288.;
pub(super) const QR_FRAME_SIDE: f32 = QR_INLINE_SIDE + 2.;
pub(super) const QR_FRAME_RADIUS: f32 = 14.;
const QR_CODE_RADIUS: f32 = QR_FRAME_RADIUS - 1.;
const UNAVAILABLE_ICON_SIDE: f32 = 42.;
const HALO_BLUR: f32 = 6.;

/// Where a slot was last laid out, for motion that ends in it.
pub type SlotAnchor = Rc<Cell<Option<Bounds<Pixels>>>>;

#[derive(IntoElement)]
pub struct QrSlot {
    id: ElementId,
    face: Face,
    anchor: Option<SlotAnchor>,
}

#[derive(Clone)]
struct Face {
    code: Option<QrCodeImage>,
    /// A link is still expected.
    pending: bool,
    arrival: Arrival,
    still: bool,
    unavailable: SharedString,
}

impl Face {
    const fn signal(&self) -> Signal {
        Signal {
            code: self.code.is_some(),
            pending: self.pending,
            still: self.still,
            arrival: self.arrival,
        }
    }
}

impl QrSlot {
    pub fn new(
        id: impl Into<ElementId>,
        code: Option<QrCodeImage>,
        pending: bool,
        unavailable: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            face: Face {
                code,
                pending,
                arrival: Arrival::Whirl(None),
                still: false,
                unavailable: unavailable.into(),
            },
            anchor: None,
        }
    }

    pub const fn arrival(mut self, arrival: Arrival) -> Self {
        self.face.arrival = arrival;
        self
    }

    pub fn anchor(mut self, anchor: &SlotAnchor) -> Self {
        self.anchor = Some(Rc::clone(anchor));
        self
    }

    /// Reduced motion: the wait is a picture and the code simply appears.
    pub const fn still(mut self, still: bool) -> Self {
        self.face.still = still;
        self
    }

    pub const fn has_code(&self) -> bool {
        self.face.code.is_some()
    }
}

impl RenderOnce for QrSlot {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let Self { id, face, anchor } = self;
        let view = window.use_keyed_state(id.clone(), cx, {
            let face = face.clone();
            move |window, cx| SlotView::new(id, face, window, cx)
        });
        view.update(cx, |view, _| view.present(face, anchor));
        view
    }
}

/// What the slot's canvas draws for one frame.
enum Painting {
    Standing(Standing),
    Bloom {
        standing: Standing,
        code: QrCodeImage,
        progress: f32,
    },
}

struct SlotView {
    id: ElementId,
    face: Face,
    phase: Phase,
    anchor: Option<SlotAnchor>,
    _subscriptions: [Subscription; 2],
}

impl SlotView {
    fn new(id: ElementId, face: Face, window: &mut Window, cx: &Context<Self>) -> Self {
        let phase = Phase::enter(face.signal(), Instant::now());
        tracing::info!(
            event = "share.qr_slot_mounted",
            session = std::process::id(),
            slot = ?id,
            ?phase,
        );
        Self {
            id,
            face,
            phase,
            anchor: None,
            // A hidden window stops asking for frames. Becoming visible again
            // must restart a wait that is still running.
            _subscriptions: [
                cx.observe_window_visibility(window, |_, _, _, cx| cx.notify()),
                cx.observe_window_activation(window, |_, _, cx| cx.notify()),
            ],
        }
    }

    fn present(&mut self, face: Face, anchor: Option<SlotAnchor>) {
        let next = self.phase.advance(face.signal(), Instant::now());
        if std::mem::discriminant(&next) != std::mem::discriminant(&self.phase) {
            tracing::info!(
                event = "share.qr_slot_phase",
                session = std::process::id(),
                slot = ?self.id,
                from = ?self.phase,
                to = ?next,
            );
        }
        self.phase = next;
        self.face = face;
        self.anchor = anchor;
    }

    /// The frame's motion and whether another frame has to follow it.
    fn painting(&self, now: Instant) -> (Option<Painting>, bool) {
        match self.phase {
            Phase::Gathering { .. } if self.face.still => {
                (Some(Painting::Standing(Standing::STILL)), false)
            }
            Phase::Gathering { wind } => (wind.standing(now).map(Painting::Standing), true),
            Phase::Blooming { wind, since } => (
                self.face.code.clone().map(|code| Painting::Bloom {
                    standing: wind.standing(now).unwrap_or(Standing::STILL),
                    code,
                    progress: (seconds_between(since, now) / BLOOM).min(1.),
                }),
                true,
            ),
            Phase::Fading { .. } => (None, true),
            Phase::Settled { .. } | Phase::Unavailable | Phase::Waiting => (None, false),
        }
    }
}

impl Render for SlotView {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = Theme::current(window, cx);
        let now = Instant::now();
        let (painting, moving) = self.painting(now);
        let halo = match self.phase {
            Phase::Settled { bloomed: Some(at) } => bloom::halo(seconds_between(at, now)),
            _ => None,
        };
        let animating = moving || halo.is_some();
        let anchor = self.anchor.clone();
        let tones = Tones::of(theme);
        let motion = canvas(
            move |bounds, window, _| {
                if let Some(anchor) = &anchor {
                    anchor.set(Some(bounds));
                }
                if animating && window.is_visible() && window.is_window_active() {
                    window.request_animation_frame();
                }
            },
            move |bounds, (), window, _| match painting {
                Some(Painting::Standing(standing)) => {
                    whirl::paint_standing(window, tones, bounds, standing);
                }
                Some(Painting::Bloom {
                    standing,
                    code,
                    progress,
                }) => whirl::paint_bloom(
                    window,
                    tones,
                    bounds,
                    standing,
                    &Opening {
                        code: &code,
                        corner: QR_CODE_RADIUS,
                        progress,
                    },
                ),
                None => {}
            },
        )
        .absolute()
        .inset_0();
        let presence = match self.phase {
            Phase::Settled { .. } => 1.,
            Phase::Fading { since } => smoothstep(0., FADE, seconds_between(since, now)),
            _ => 0.,
        };
        let settled = self.face.code.as_ref().filter(|_| presence > 0.);
        column()
            .relative()
            // Reserve the largest inline size before encoding. Link density must not
            // resize the card or switch its columns at the moment it becomes usable.
            .size(px(QR_FRAME_SIDE))
            .flex_shrink_0()
            .border_1()
            .border_color(theme.border())
            .rounded(px(QR_FRAME_RADIUS))
            .overflow_hidden()
            .bg(theme.canvas())
            .when_some(halo, |el, (spread, opacity)| {
                el.shadow(vec![BoxShadow {
                    color: theme.action_surface().opacity(opacity),
                    offset: point(px(0.), px(0.)),
                    blur_radius: px(HALO_BLUR),
                    spread_radius: px(spread),
                    inset: false,
                }])
            })
            .child(motion)
            .when(
                matches!(self.phase, Phase::Waiting | Phase::Fading { .. }),
                |el| el.child(waiting_qr(theme).opacity(1. - presence)),
            )
            .when_some(settled, |el, code| {
                el.child(
                    div()
                        .absolute()
                        .inset_0()
                        .opacity(presence)
                        .child(code.view(QR_INLINE_SIDE).rounded(px(QR_CODE_RADIUS))),
                )
            })
            .when(self.phase == Phase::Unavailable, |el| {
                el.child(
                    column()
                        .size_full()
                        .items_center()
                        .justify_center()
                        .gap(px(12.))
                        .child(
                            icon(Icon::QrCode, UNAVAILABLE_ICON_SIDE)
                                .text_color(theme.muted_text()),
                        )
                        .child(theme.caption(self.face.unavailable.clone())),
                )
            })
    }
}
