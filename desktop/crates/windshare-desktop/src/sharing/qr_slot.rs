//! The QR frame of a share. While the link is pending it holds the eye of a
//! whirl; when the link arrives the eye opens into the code.

use crate::motion::whirl::{
    self, Opening, Standing, Tones, Wind,
    bloom::{self, BLOOM},
    seconds_between,
};
use gpui::{
    App, Bounds, BoxShadow, Context, ElementId, IntoElement, Pixels, SharedString, Subscription,
    Window, canvas, div, point, prelude::*, px,
};
use std::{
    cell::Cell,
    rc::Rc,
    time::{Duration, Instant},
};
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
    wind: Option<Wind>,
    still: bool,
    unavailable: SharedString,
}

impl Face {
    const fn signal(&self) -> Signal {
        Signal {
            code: self.code.is_some(),
            pending: self.pending,
            still: self.still,
            wind: self.wind,
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
                wind: None,
                still: false,
                unavailable: unavailable.into(),
            },
            anchor: None,
        }
    }

    /// The whirl a run already raised for this slot. Without one the slot
    /// raises its own when it first has to wait.
    pub const fn wind(mut self, wind: Wind) -> Self {
        self.face.wind = Some(wind);
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

/// What a slot needs to know to move between phases.
#[derive(Clone, Copy, Debug)]
struct Signal {
    code: bool,
    pending: bool,
    still: bool,
    wind: Option<Wind>,
}

#[derive(Clone, Copy, Debug, PartialEq)]
enum Phase {
    /// A whirl stands in the slot, or is on its way, while the link is pending.
    Gathering { wind: Wind },
    /// The eye is opening into the code.
    Blooming { wind: Wind, since: Instant },
    /// The static code. `bloomed` is when a bloom ended here, for its halo; a
    /// code that was simply present has none.
    Settled { bloomed: Option<Instant> },
    /// No link to encode and none expected.
    Unavailable,
}

impl Phase {
    fn enter(signal: Signal, now: Instant) -> Self {
        if signal.code {
            Self::Settled { bloomed: None }
        } else if signal.pending {
            Self::Gathering {
                wind: signal.wind.unwrap_or(Wind::Rooted { since: now }),
            }
        } else {
            Self::Unavailable
        }
    }

    fn advance(self, signal: Signal, now: Instant) -> Self {
        match self {
            // A code can only bloom out of a whirl that already stands here.
            // One still in flight keeps the slot waiting for its arrival.
            Self::Gathering { wind } if signal.code && !signal.still => {
                if wind.standing(now).is_some() {
                    Self::Blooming { wind, since: now }
                } else {
                    self
                }
            }
            Self::Gathering { .. } if !signal.code && signal.pending => self,
            Self::Blooming { since, .. } if signal.code => {
                if seconds_between(since, now) < BLOOM {
                    self
                } else {
                    Self::Settled {
                        bloomed: Some(since + Duration::from_secs_f32(BLOOM)),
                    }
                }
            }
            Self::Settled { .. } if signal.code => self,
            Self::Unavailable if !signal.code && !signal.pending => self,
            _ => Self::enter(signal, now),
        }
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
    _visibility: Subscription,
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
            _visibility: cx.observe_window_visibility(window, |_, _, _, cx| cx.notify()),
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
            Phase::Settled { .. } | Phase::Unavailable => (None, false),
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
                if animating && window.is_visible() {
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
        let settled = match self.phase {
            Phase::Settled { .. } => self.face.code.as_ref(),
            _ => None,
        };
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
            .when_some(settled, |el, code| {
                el.child(
                    div()
                        .absolute()
                        .inset_0()
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

#[cfg(test)]
#[path = "qr_slot_tests.rs"]
mod tests;
