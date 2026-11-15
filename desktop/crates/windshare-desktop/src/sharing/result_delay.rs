use crate::{
    desktop::Desktop,
    flow_state::{ShareFlow, ShareRun},
    model::Page,
};
use gpui::{Context, Window};
use std::time::Instant;

impl Desktop {
    pub(crate) fn present_share(
        &mut self,
        run: ShareRun,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.share_flow = ShareFlow::Running(run);
        self.share_result_wakeup = None;
        let delay = run.remaining(Instant::now());
        tracing::info!(
            event = "share.result_wait_started",
            session = std::process::id(),
            share_id = run.share_id,
            wait_ms = delay.as_millis(),
        );
        #[cfg(feature = "motion-profile")]
        crate::profiling::mark(
            run.share_id,
            if delay.is_zero() { "Result" } else { "Waiting" },
        );
        if !delay.is_zero() {
            // One wakeup leaves the static waiting page idle. A replaced selection
            // cancels this task so an older operation cannot reveal the new result.
            self.share_result_wakeup = Some(cx.spawn_in(window, async move |view, cx| {
                cx.background_executor().timer(delay).await;
                let _ = view.update_in(cx, |this, window, cx| {
                    if !matches!(this.share_flow, ShareFlow::Running(current) if current.share_id == run.share_id) {
                        return;
                    }
                    let link_ready = this.shares.iter().any(|share| {
                        share.id == run.share_id && share.usable_link().is_some()
                    });
                    tracing::info!(
                        event = "share.result_wait_finished",
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
}
