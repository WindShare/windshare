//! A share run's entrance, captured with its animation preference and launch pose.
//!
//! The launch pose and the whirl's destination are measured where they were
//! painted, so the handover holds in any window.

use super::qr_slot::SlotAnchor;
use crate::{
    desktop::Desktop,
    flow_state::{ShareFlow, ShareRun},
    model::Page,
    motion::{
        ShareAnimation, SpiritAnchor, SpiritPlacement, frame_ticker, paper, vacated_scene,
        whirl::{
            self, Tones,
            easing::{ease_out_quint, smoothstep},
            flight::{HOME, WINDUP},
        },
    },
    preferences::{PreferenceChoice, ShareAnimationSettings},
};
use gpui::{
    AnyElement, App, Bounds, Context, IntoElement, Pixels, Window, anchored, canvas, point,
    prelude::*, px,
};
use std::{
    cell::Cell,
    rc::Rc,
    time::{Duration, Instant},
};
use windshare_ui::{Theme, Tone, column};

/// Seconds the result card takes to arrive.
const CARD_ENTRANCE: f32 = 0.42;
/// Pixels the card rises while it arrives.
const CARD_RISE: f32 = 12.;
// Let the spirit leave the text's area before bringing that information forward.
const INFORMATION_ENTRANCE: (f32, f32) = (0.30, 0.82);

/// Where the choose page last painted its prompt, in window coordinates.
#[derive(Clone, Copy, Debug)]
pub struct Perch {
    /// The whole prompt column, so a departing copy lands on the same pixels.
    pub prompt: Bounds<Pixels>,
    pub scene_scale: f32,
    pub spirit: SpiritPlacement,
}

/// Places measured during paint that a run's entrance moves between.
#[derive(Default)]
pub struct IntroAnchors {
    pub perch: Rc<Cell<Option<Perch>>>,
    pub spirit: SpiritAnchor,
    pub slot: SlotAnchor,
}

impl IntroAnchors {
    /// An anchor only exists while its element is on screen, so every frame
    /// measures anew and a stale place can never start or end a flight.
    pub fn begin_frame(&self) {
        self.perch.set(None);
        self.spirit.set(None);
        self.slot.set(None);
    }
}

/// One frame of a run's entrance, derived from its clock alone.
#[derive(Clone, Copy)]
pub(super) struct IntroFrame {
    pub run: ShareRun,
    seconds: f32,
    /// Reduced motion presents every run as already arrived.
    pub still: bool,
}

impl IntroFrame {
    /// Whether the card may present more than "preparing".
    pub fn revealed(self) -> bool {
        self.still || self.seconds >= self.run.animation.duration()
    }

    pub fn card_presence(self) -> f32 {
        if self.still {
            return 1.;
        }
        if self.run.animation == ShareAnimation::Paper {
            return paper::card_presence(self.seconds);
        }
        // A card arriving under a spirit that is still gathering itself would upstage the takeoff.
        let delay = if self.run.perch.is_some() { WINDUP } else { 0. };
        ease_out_quint((self.seconds - delay) / CARD_ENTRANCE)
    }

    pub fn card_rise(self) -> f32 {
        CARD_RISE * (1. - self.card_presence())
    }

    pub fn information_presence(self) -> f32 {
        if self.still {
            1.
        } else {
            match self.run.animation {
                ShareAnimation::Whirl => {
                    smoothstep(INFORMATION_ENTRANCE.0, INFORMATION_ENTRANCE.1, self.seconds)
                }
                ShareAnimation::Paper => smoothstep(
                    INFORMATION_ENTRANCE.0,
                    paper::LANDING,
                    self.seconds - paper::FLIGHT,
                ),
            }
        }
    }

    const fn running(self) -> bool {
        !self.still && self.seconds < self.run.animation.duration()
    }

    /// The prompt the spirit is leaving and how much of it is still there. It
    /// is gone by the time the spirit takes off.
    fn departing(self) -> Option<(Perch, f32)> {
        let duration = match self.run.animation {
            ShareAnimation::Whirl => WINDUP,
            ShareAnimation::Paper => paper::PROMPT_DEPARTURE,
        };
        self.run
            .perch
            .filter(|_| self.running() && self.seconds < duration)
            .map(|perch| (perch, 1. - smoothstep(0., duration, self.seconds)))
    }

    fn flying(self) -> Option<Perch> {
        self.run.perch.filter(|_| {
            self.run.animation == ShareAnimation::Whirl && self.running() && self.seconds < HOME
        })
    }
}

impl Desktop {
    /// A run starts from the spirit's perch when the user was looking at it.
    pub(crate) fn share_run(&self, share_id: usize, cx: &App) -> ShareRun {
        ShareRun::new(
            share_id,
            Instant::now(),
            self.intro_anchors.perch.get(),
            cx.global::<ShareAnimationSettings>().choice,
        )
    }

    pub(crate) fn present_share(
        &mut self,
        run: ShareRun,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.share_flow = ShareFlow::Running(run);
        self.share_reveal_wakeup = None;
        let wait = if self.still(cx) {
            Duration::ZERO
        } else {
            run.until_reveal(Instant::now())
        };
        tracing::info!(
            event = "share.intro_started",
            session = std::process::id(),
            share_id = run.share_id,
            carried = run.perch.is_some(),
            animation = run.animation.code(),
            wait_ms = wait.as_millis(),
        );
        #[cfg(feature = "motion-profile")]
        crate::profiling::mark(
            run.share_id,
            if wait.is_zero() { "Result" } else { "Intro" },
        );
        if !wait.is_zero() {
            // The reveal itself is a function of the clock; this wakeup moves
            // keyboard focus onto the copy action it enables. A replaced run
            // cancels the task so an older run cannot steal the new one's focus.
            self.share_reveal_wakeup = Some(cx.spawn_in(window, async move |view, cx| {
                cx.background_executor().timer(wait).await;
                let _ = view.update_in(cx, |this, window, cx| {
                    if !matches!(this.share_flow, ShareFlow::Running(current) if current.share_id == run.share_id) {
                        return;
                    }
                    let link_ready = this.shares.iter().any(|share| {
                        share.id == run.share_id && share.usable_link().is_some()
                    });
                    tracing::info!(
                        event = "share.intro_finished",
                        session = std::process::id(),
                        share_id = run.share_id,
                        link_ready,
                    );
                    #[cfg(feature = "motion-profile")]
                    crate::profiling::mark(run.share_id, "Result");
                    if this.page == Page::Share && this.overlays.is_empty() {
                        this.focus_task(window, cx);
                    }
                    cx.notify();
                });
            }));
        }
        self.focus_task(window, cx);
        cx.notify();
    }

    pub(super) fn share_intro(&self, run: ShareRun, cx: &App) -> IntroFrame {
        IntroFrame {
            run,
            seconds: run.seconds(Instant::now()),
            still: self.still(cx),
        }
    }

    /// What is drawn over the result card while its run arrives.
    pub(super) fn intro_layers(
        &self,
        theme: Theme,
        intro: IntroFrame,
        cx: &Context<Self>,
    ) -> Vec<AnyElement> {
        let mut layers = Vec::new();
        if !intro.running() {
            return layers;
        }
        // The card's entrance and its reveal are read from the clock on every
        // render, whether or not anything here is still painting.
        layers.push(frame_ticker().into_any_element());
        if let Some((perch, presence)) = intro.departing() {
            layers.push(
                anchored()
                    .position(perch.prompt.origin)
                    .snap_to_window()
                    .child(
                        column()
                            .items_center()
                            .w(perch.prompt.size.width)
                            .opacity(presence)
                            .child(vacated_scene(Tone::Mint, perch.scene_scale))
                            .child(self.share_prompt(theme, false, cx)),
                    )
                    .into_any_element(),
            );
        }
        if let Some(perch) = intro.flying() {
            let slot = Rc::clone(&self.intro_anchors.slot);
            let tones = Tones::of(theme);
            let (seconds, rise) = (intro.seconds, intro.card_rise());
            layers.push(
                canvas(
                    |_, _, _| (),
                    move |_, (), window, _| {
                        // The slot measures itself during prepaint. A card that
                        // shows none leaves the flight nowhere to go.
                        let Some(slot) = slot.get() else {
                            return;
                        };
                        // Aim at where the rising card comes to rest.
                        let settled = slot - point(px(0.), px(rise));
                        whirl::paint_flight(window, tones, perch.spirit, settled, seconds);
                    },
                )
                .absolute()
                .inset_0()
                .into_any_element(),
            );
        }
        if intro.run.animation == ShareAnimation::Paper {
            layers.push(
                paper::scenery(intro.run.perch.map(|perch| perch.spirit), intro.seconds)
                    .into_any_element(),
            );
        }
        layers
    }
}
