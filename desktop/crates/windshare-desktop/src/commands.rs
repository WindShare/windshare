use crate::localization::{LanguageSettings, Text};
use crate::{
    desktop::Desktop,
    flow_state::{
        COPY_FEEDBACK_MS, CopyFeedback, LinkFeedback, RECEIVE_OPEN_MS, RECEIVE_SAVE_MS,
        ReceiveFlow, SampleBundle, ShareFlow, ShareReadiness,
    },
    model::{Action, Overlay, Page, ShareState, TransferState},
};
use gpui::{BorrowAppContext, ClipboardItem, Context, Focusable, Window};
use std::time::Duration;

const DEMO_LINK: &str = "https://windshare.example/s/kyoto-weekend";

impl Desktop {
    pub fn dispatch(&mut self, action: Action, window: &mut Window, cx: &mut Context<Self>) {
        tracing::info!(operation = "desktop.action", session = std::process::id(), action = ?action, preview = self.runtime.is_preview());
        match action {
            Action::Navigate(page) => self.navigate(page, window, cx),
            Action::NewShare => {
                self.share_flow = ShareFlow::Choose;
                self.navigate(Page::Share, window, cx);
            }
            Action::Receive => self.navigate(Page::Receive, window, cx),
            Action::SelectFiles => {
                if self.runtime.is_preview() {
                    self.prepare_preview_share(SampleBundle::Design, window, cx);
                } else {
                    self.choose_native(false, window, cx);
                }
            }
            Action::SelectFolder => {
                if self.runtime.is_preview() {
                    self.prepare_preview_share(SampleBundle::Photos, window, cx);
                } else {
                    self.choose_native(true, window, cx);
                }
            }
            Action::ResetShare => self.reset_share(window, cx),
            Action::DemoLink => {
                self.link_input
                    .update(cx, |input, cx| input.set_value(DEMO_LINK, cx));
                self.open_preview(window, cx);
            }
            Action::OpenPreview => self.open_preview(window, cx),
            Action::PasteLink => {
                if let Some(text) = cx.read_from_clipboard().and_then(|item| item.text()) {
                    self.link_input
                        .update(cx, |input, cx| input.set_value(text, cx));
                }
                let focus = self.link_input.read(cx).focus_handle(cx);
                window.focus(&focus, cx);
            }
            Action::SavePreview => self.save_preview(window, cx),
            Action::ResetReceive => {
                self.receive_flow = ReceiveFlow::default();
                self.link_input
                    .update(cx, |input, cx| input.set_value("", cx));
                self.focus_task(window, cx);
            }
            Action::Filter(filter) => self.filter = filter,
            Action::ToggleGrid => self.grid = !self.grid,
            Action::Detail(id) => self.open_overlay(Overlay::Detail(id), window, cx),
            Action::Help => self.open_overlay(Overlay::Help, window, cx),
            Action::Close => self.close_overlay(window, cx),
            Action::CopyLink(id) => self.copy_link(id, cx),
            Action::ToggleLink(id) => self.toggle_link(id),
            Action::EnlargeQr(id) => self.enlarge_qr(id, window, cx),
            Action::StopShare(id) => self.stop_native_share(id),
            Action::ShareAgain(id) => self.repeat_native_share(id, window, cx),
            Action::ConfigureShare(id) => self.open_sharing_setup(id, window, cx),
            Action::SaveSharingSetup(id) => self.save_sharing_setup(id, cx),
            Action::ResumeShare(id) => self.resume_native_share(id),
            Action::ApproveRelay => self.approve_relay(cx),
            Action::OpenLink(id) => {
                if let Some(link) = self
                    .shares
                    .iter()
                    .find(|share| share.id == id)
                    .and_then(crate::model::Share::usable_link)
                {
                    cx.open_url(&link.url());
                }
            }
            Action::ToggleShare(id) => self.toggle_preview_share(id),
            Action::ToggleTransfer(index) => {
                if let Some(transfer) = self.transfers.get_mut(index) {
                    transfer.state = match transfer.state {
                        TransferState::Running => TransferState::Paused,
                        TransferState::Paused => TransferState::Running,
                        TransferState::Complete => TransferState::Complete,
                    };
                }
            }
            Action::ToggleSetting(index) => {
                if let Some(setting) = self.settings.get_mut(index) {
                    *setting = !*setting;
                    self.super_share.update(cx, |view, cx| {
                        view.set_reduced_motion(self.settings[2], cx);
                    });
                }
            }
            Action::SetAppearance(choice) => crate::preferences::select_appearance(choice, cx),
            Action::SetLanguage(choice) => {
                cx.update_global::<LanguageSettings, _>(|settings, _| settings.select(choice));
            }
            Action::Toast(message) => self.notify(message, cx),
        }
        cx.notify();
    }

    fn toggle_preview_share(&mut self, id: usize) {
        if !self.runtime.is_preview() {
            return;
        }
        if let Some(share) = self.shares.iter_mut().find(|share| share.id == id) {
            share.state = if share.live() {
                ShareState::Paused
            } else {
                ShareState::Live { viewers: Some(0) }
            };
        }
    }

    fn reset_share(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if let ShareFlow::Running(run) = self.share_flow
            && matches!(run.readiness, ShareReadiness::Pending)
        {
            self.stop_native_share(run.selection_id);
            if self.runtime.is_preview()
                && let Some(share) = self
                    .shares
                    .iter_mut()
                    .find(|share| share.id == run.selection_id)
            {
                share.state = ShareState::Cancelled;
            }
        }
        self.share_flow = ShareFlow::Choose;
        self.focus_task(window, cx);
    }

    fn copy_link(&mut self, id: usize, cx: &mut Context<Self>) {
        let Some(share) = self.shares.iter().find(|share| share.id == id) else {
            return;
        };
        let Some(link) = share.usable_link() else {
            return;
        };
        cx.write_to_clipboard(ClipboardItem::new_string(link.url().to_string()));
        let revision = self.next_operation();
        self.copy_feedback = CopyFeedback::Copied {
            share_id: id,
            revision,
        };
        let inline_feedback = (self.page == Page::Share
            && self.overlays.is_empty()
            && matches!(self.share_flow, ShareFlow::Running(run) if run.share_id() == Some(id)))
            || self.overlays.last().is_some_and(
                |frame| matches!(frame.content, Overlay::Detail(share_id) if share_id == id),
            );
        // The result button acknowledges its own action; other surfaces still
        // need a toast because they have no inline confirmation.
        if !inline_feedback {
            self.notify(Text::LinkCopied, cx);
        }
        tracing::info!(
            event = "share.link_copied",
            session = std::process::id(),
            operation = revision,
            share_id = id,
            inline_feedback
        );
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

    pub const fn next_operation(&mut self) -> usize {
        self.operation_sequence += 1;
        self.operation_sequence
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
