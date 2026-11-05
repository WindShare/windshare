use crate::{
    flow_state::ShareRun,
    share_timing::{PresentationPhase, ShareFrame},
};
use std::time::Instant;

const SAMPLE_LIMIT: usize = 2048;
const SLOW_CALLBACK_MS: f32 = 25.;

#[derive(Default)]
pub struct MotionProbe {
    operation: Option<usize>,
    phase: Option<PresentationPhase>,
    previous: Option<Instant>,
    intervals: Vec<f32>,
}
impl MotionProbe {
    pub fn observe(&mut self, run: ShareRun, frame: ShareFrame, now: Instant, animating: bool) {
        if self.operation != Some(run.operation) {
            *self = Self {
                operation: Some(run.operation),
                ..Self::default()
            };
        }
        if self.phase == Some(PresentationPhase::Complete) {
            return;
        }
        if animating {
            if let Some(previous) = self.previous
                && self.intervals.len() < SAMPLE_LIMIT
            {
                self.intervals
                    .push(now.duration_since(previous).as_secs_f32() * 1000.);
            }
            self.previous = Some(now);
        } else {
            // Hidden windows, reduced motion and completed scenes do not schedule frames.
            self.previous = None;
        }
        if self.phase != Some(frame.phase) {
            #[cfg(feature = "motion-profile")]
            crate::profiling::mark(run.operation, format!("{:?}", frame.phase));
            tracing::info!(event = "share.presentation_phase", operation = run.operation,
                phase = ?frame.phase,
                animating,
                elapsed_ms = now.duration_since(run.started_at).as_millis(),
                link_ready = run.share_id().is_some());
            self.phase = Some(frame.phase);
            if frame.phase == PresentationPhase::Complete && !self.intervals.is_empty() {
                self.intervals.sort_by(f32::total_cmp);
                let count = self.intervals.len();
                // Callback cadence diagnoses UI scheduling; it is not GPU presentation FPS.
                tracing::info!(
                    event = "share.render_cadence",
                    operation = run.operation,
                    samples = count,
                    p95_callback_ms = self.intervals[(count * 95).div_ceil(100) - 1],
                    max_callback_ms = self.intervals[count - 1],
                    callbacks_over_25_ms = self
                        .intervals
                        .iter()
                        .filter(|&&ms| ms > SLOW_CALLBACK_MS)
                        .count()
                );
            }
        }
    }
}
