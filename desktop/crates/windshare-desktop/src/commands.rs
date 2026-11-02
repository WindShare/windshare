use crate::{
    desktop::Desktop,
    flow_state::*,
    model::*,
    share_timing::{MIN_FLIGHT_DURATION, ShareArrival},
};
use gpui::*;
use std::time::{Duration, Instant};

const DEMO_LINK: &str = "https://windshare.example/s/kyoto-weekend";

impl Desktop {
    pub fn dispatch(&mut self, action: PreviewAction, window: &mut Window, cx: &mut Context<Self>) {
        tracing::info!(operation = "preview.action", session = std::process::id(), action = ?action);
        match action {
            PreviewAction::Navigate(page) => self.navigate(page, window, cx),
            PreviewAction::NewShare => {
                self.share_flow = ShareFlow::Choose;
                self.navigate(Page::Share, window, cx);
            }
            PreviewAction::Receive => self.navigate(Page::Receive, window, cx),
            PreviewAction::SelectFiles => self.prepare_share(SampleBundle::Design, window, cx),
            PreviewAction::SelectFolder => self.prepare_share(SampleBundle::Photos, window, cx),
            PreviewAction::ResetShare => {
                self.share_flow = ShareFlow::Choose;
                self.focus_task(window, cx);
            }
            PreviewAction::DemoLink => {
                self.link_input
                    .update(cx, |input, cx| input.set_value(DEMO_LINK, cx));
                self.open_preview(window, cx);
            }
            PreviewAction::OpenPreview => self.open_preview(window, cx),
            PreviewAction::PasteLink => {
                if let Some(text) = cx.read_from_clipboard().and_then(|item| item.text()) {
                    self.link_input
                        .update(cx, |input, cx| input.set_value(text, cx));
                }
                let focus = self.link_input.read(cx).focus_handle(cx);
                window.focus(&focus, cx);
            }
            PreviewAction::SavePreview => self.save_preview(window, cx),
            PreviewAction::ResetReceive => {
                self.receive_flow = ReceiveFlow::default();
                self.link_input
                    .update(cx, |input, cx| input.set_value("", cx));
                self.focus_task(window, cx);
            }
            PreviewAction::Filter(filter) => self.filter = filter,
            PreviewAction::ToggleGrid => self.grid = !self.grid,
            PreviewAction::Detail(id) => self.open_overlay(Overlay::Detail(id), window, cx),
            PreviewAction::Help => self.open_overlay(Overlay::Help, window, cx),
            PreviewAction::Close => self.close_overlay(window, cx),
            PreviewAction::CopyLink(id) => self.copy_link(id, cx),
            PreviewAction::ToggleShare(id) => {
                if let Some(share) = self.shares.iter_mut().find(|share| share.id == id) {
                    share.state = if share.live() {
                        ShareState::Paused
                    } else {
                        ShareState::Live { viewers: 0 }
                    };
                }
            }
            PreviewAction::ToggleTransfer(index) => {
                if let Some(transfer) = self.transfers.get_mut(index) {
                    transfer.state = match transfer.state {
                        TransferState::Running => TransferState::Paused,
                        TransferState::Paused => TransferState::Running,
                        TransferState::Complete => TransferState::Complete,
                    };
                }
            }
            PreviewAction::ToggleSetting(index) => {
                if let Some(setting) = self.settings.get_mut(index) {
                    *setting = !*setting;
                }
            }
            PreviewAction::Toast(message) => self.notify(message, cx),
        }
        cx.notify();
    }

    fn copy_link(&mut self, id: usize, cx: &mut Context<Self>) {
        let Some(share) = self.shares.iter().find(|share| share.id == id) else {
            return;
        };
        cx.write_to_clipboard(ClipboardItem::new_string(share.link()));
        let revision = self.next_operation();
        self.copy_feedback = CopyFeedback::Copied {
            share_id: id,
            revision,
        };
        self.notify("分享链接已复制", cx);
        cx.spawn(async move |view, cx| {
            cx.background_executor().timer(Duration::from_millis(COPY_FEEDBACK_MS)).await;
            let _ = view.update(cx, |this, cx| {
                if matches!(this.copy_feedback, CopyFeedback::Copied { revision: current, .. } if current == revision) {
                    this.copy_feedback = CopyFeedback::Idle;
                    cx.notify();
                }
            });
        }).detach();
    }

    fn next_operation(&mut self) -> usize {
        self.operation_sequence += 1;
        self.operation_sequence
    }

    fn prepare_share(&mut self, bundle: SampleBundle, window: &mut Window, cx: &mut Context<Self>) {
        let operation = self.next_operation();
        let preview_delay = self.preview_share_delay;
        self.share_flow = ShareFlow::Preparing {
            operation,
            bundle,
            started_at: Instant::now(),
        };
        self.focus_task(window, cx);
        tracing::info!(
            event = "share.preparing",
            operation,
            session = std::process::id(),
            preview_delay_ms = preview_delay.as_millis(),
            ?bundle,
            preview = true
        );
        cx.spawn(async move |view, cx| {
            // Only the preview source knows the simulated latency; motion never predicts readiness.
            cx.background_executor().timer(preview_delay).await;
            let Some(entrance_remaining) = view
                .update(cx, |this, cx| this.share_prepared(operation, bundle, cx))
                .ok()
                .flatten()
            else {
                return;
            };
            if !entrance_remaining.is_zero() {
                cx.background_executor().timer(entrance_remaining).await;
            }
            let settling = view
                .update(cx, |this, cx| this.begin_share_reveal(operation, cx))
                .ok()
                .flatten();
            if let Some(duration) = settling {
                cx.background_executor().timer(duration).await;
                let _ = view.update(cx, |this, cx| {
                    // A replay or cancellation owns a new operation and must not inherit this ending.
                    if let ShareFlow::Revealing {
                        operation: current,
                        share_id,
                        ..
                    } = this.share_flow
                        && current == operation
                    {
                        this.share_flow = ShareFlow::Ready { share_id };
                        tracing::info!(event = "share.presentation_settled", operation, share_id);
                        cx.notify();
                    }
                });
            }
        })
        .detach();
    }

    fn share_prepared(
        &mut self,
        operation: usize,
        bundle: SampleBundle,
        cx: &mut Context<Self>,
    ) -> Option<Duration> {
        let ShareFlow::Preparing {
            operation: current,
            started_at,
            ..
        } = self.share_flow
        else {
            tracing::info!(
                event = "share.completion_discarded",
                operation,
                reason = "no_longer_preparing"
            );
            return None;
        };
        if current != operation {
            tracing::info!(
                event = "share.completion_discarded",
                operation,
                current,
                reason = "superseded"
            );
            return None;
        }
        let elapsed = started_at.elapsed();
        let entrance_remaining = if self.reduced_motion() || cx.reduce_motion() {
            Duration::ZERO
        } else {
            MIN_FLIGHT_DURATION.saturating_sub(elapsed)
        };
        // A ready result is retained while the minimum entrance finishes; no readiness is fabricated.
        self.share_flow = ShareFlow::AwaitingReveal {
            operation,
            bundle,
            started_at,
        };
        tracing::info!(
            event = "share.ready",
            operation,
            elapsed_ms = elapsed.as_millis(),
            entrance_remaining_ms = entrance_remaining.as_millis(),
            preview = true
        );
        cx.notify();
        Some(entrance_remaining)
    }

    fn begin_share_reveal(&mut self, operation: usize, cx: &mut Context<Self>) -> Option<Duration> {
        let ShareFlow::AwaitingReveal {
            operation: current,
            bundle,
            started_at,
        } = self.share_flow
        else {
            tracing::info!(
                event = "share.presentation_discarded",
                operation,
                reason = "no_longer_waiting"
            );
            return None;
        };
        if current != operation {
            tracing::info!(
                event = "share.presentation_discarded",
                operation,
                current,
                reason = "superseded"
            );
            return None;
        }
        let reveal_at = Instant::now();
        let arrival = ShareArrival::new(started_at, reveal_at);
        let id = self.shares.iter().map(|share| share.id).max().unwrap_or(0) + 1;
        self.shares.insert(
            0,
            Share {
                id,
                name: bundle.name().into(),
                detail: bundle.detail(),
                size: bundle.size(),
                date: "刚刚",
                tone: bundle.tone(),
                symbol: bundle.icon(),
                state: ShareState::Live { viewers: 0 },
            },
        );
        let animate = !self.reduced_motion() && !cx.reduce_motion();
        // The minimum flight is complete; copying works throughout the short landing.
        self.share_flow = if animate {
            ShareFlow::Revealing {
                operation,
                share_id: id,
                arrival,
            }
        } else {
            ShareFlow::Ready { share_id: id }
        };
        tracing::info!(
            event = "share.presentation_started",
            operation,
            share_id = id,
            elapsed_ms = reveal_at.duration_since(started_at).as_millis(),
            settle_ms = if animate {
                arrival.duration().as_millis()
            } else {
                0
            },
            preview = true
        );
        cx.notify();
        animate.then_some(arrival.duration())
    }

    fn open_preview(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if !matches!(self.receive_flow, ReceiveFlow::Link { .. }) {
            return;
        }
        let link = self.link_input.read(cx).value().trim();
        if !link.starts_with("https://") || !link.contains("/s/") {
            self.receive_flow = ReceiveFlow::Link {
                feedback: LinkFeedback::Invalid,
            };
            self.focus_task(window, cx);
            return;
        }
        let operation = self.next_operation();
        self.receive_flow = ReceiveFlow::Opening { operation };
        self.focus_task(window, cx);
        tracing::info!(
            event = "receive.opening",
            operation,
            session = std::process::id(),
            preview = true
        );
        cx.spawn(async move |view, cx| {
            cx.background_executor().timer(Duration::from_millis(RECEIVE_OPEN_MS)).await;
            let _ = view.update(cx, |this, cx| {
                if matches!(this.receive_flow, ReceiveFlow::Opening { operation: current } if current == operation) {
                    this.receive_flow = ReceiveFlow::Files;
                    tracing::info!(event = "receive.files_ready", operation, preview = true);
                    cx.notify();
                }
            });
        }).detach();
    }

    fn save_preview(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if !matches!(self.receive_flow, ReceiveFlow::Files) {
            return;
        }
        let operation = self.next_operation();
        self.receive_flow = ReceiveFlow::Saving { operation };
        self.focus_task(window, cx);
        tracing::info!(
            event = "receive.saving",
            operation,
            session = std::process::id(),
            preview = true
        );
        cx.spawn(async move |view, cx| {
            cx.background_executor().timer(Duration::from_millis(RECEIVE_SAVE_MS)).await;
            let _ = view.update(cx, |this, cx| {
                if matches!(this.receive_flow, ReceiveFlow::Saving { operation: current } if current == operation) {
                    this.receive_flow = ReceiveFlow::Saved;
                    tracing::info!(event = "receive.saved_preview", operation, writes_to_disk = false);
                    cx.notify();
                }
            });
        }).detach();
    }
}
