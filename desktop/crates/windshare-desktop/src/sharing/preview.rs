use crate::{
    desktop::Desktop,
    flow_state::{SampleBundle, ShareFlow, ShareReadiness, ShareRun},
    live::Runtime,
    model::{Share, ShareAge, ShareSource, ShareState},
    sharing::{LinkDisclosure, ShareLink},
};
use gpui::{Context, Window};
use std::time::Instant;

impl Desktop {
    pub fn prepare_preview_share(
        &mut self,
        bundle: SampleBundle,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Runtime::Preview { delay } = self.runtime else {
            return;
        };
        let operation = self.next_operation();
        self.shares.insert(
            0,
            Share {
                id: operation,
                source: ShareSource::Preview,
                link: None,
                name: bundle.name().into(),
                contents: bundle.contents(),
                size: Some(bundle.size().into()),
                age: ShareAge::JustNow,
                tone: bundle.tone(),
                symbol: bundle.icon(),
                state: ShareState::Preparing,
                error: None,
                authorization: None,
                relay_status: None,
            },
        );
        self.link_disclosure = LinkDisclosure::Summary;
        self.share_flow = ShareFlow::Running(ShareRun {
            operation,
            selection_id: operation,
            started_at: Instant::now(),
            readiness: ShareReadiness::Pending,
        });
        self.focus_task(window, cx);
        tracing::info!(
            event = "share.preparing",
            operation,
            preview_delay_ms = delay.as_millis(),
            ?bundle,
            preview = true
        );
        cx.spawn(async move |view, cx| {
            cx.background_executor().timer(delay).await;
            let _ = view.update(cx, |this, cx| {
                let Some(share) = this.shares.iter_mut().find(|share| share.id == operation) else {
                    return;
                };
                if !matches!(share.state, ShareState::Preparing) {
                    return;
                }
                share.link = Some(ShareLink::preview(operation));
                share.state = ShareState::Live { viewers: Some(0) };
                if let ShareFlow::Running(run) = &mut this.share_flow
                    && run.selection_id == operation
                {
                    run.readiness = ShareReadiness::Ready { at: Instant::now() };
                }
                tracing::info!(event = "share.ready", operation, preview = true);
                cx.notify();
            });
        })
        .detach();
    }
}
