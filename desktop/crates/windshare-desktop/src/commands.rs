use crate::{desktop::Desktop, flow_state::*, model::*};
use gpui::*;
use std::time::Duration;

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
        self.share_flow = ShareFlow::Preparing { operation, bundle };
        self.focus_task(window, cx);
        tracing::info!(
            event = "share.preparing",
            operation,
            session = std::process::id(),
            ?bundle,
            preview = true
        );
        cx.spawn(async move |view, cx| {
            cx.background_executor().timer(Duration::from_millis(SHARE_PREPARE_MS)).await;
            let _ = view.update(cx, |this, cx| {
                // A canceled preview must never replace a newer choice when its timer completes.
                if !matches!(this.share_flow, ShareFlow::Preparing { operation: current, .. } if current == operation) { return; }
                let id = this.shares.iter().map(|share| share.id).max().unwrap_or(0) + 1;
                this.shares.insert(0, Share {
                    id, name: bundle.name().into(), detail: bundle.detail(), size: bundle.size(),
                    date: "刚刚", tone: bundle.tone(), symbol: bundle.icon(),
                    state: ShareState::Live { viewers: 0 },
                });
                this.share_flow = ShareFlow::Ready { share_id: id };
                tracing::info!(event = "share.ready", operation, share_id = id, preview = true);
                cx.notify();
            });
        }).detach();
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
