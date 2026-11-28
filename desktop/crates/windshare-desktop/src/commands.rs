use crate::localization::{LanguageSettings, Text};
use crate::{
    desktop::Desktop,
    flow_state::{
        COPY_FEEDBACK_MS, CopyFeedback, CopyTarget, LinkFeedback, RECEIVE_OPEN_MS, RECEIVE_SAVE_MS,
        ReceiveFlow, SampleBundle, ShareFlow,
    },
    model::{Action, Overlay, Page, ShareState, TransferState},
    sharing::{LinkKind, SharePublication},
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
                self.share_reveal_wakeup = None;
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
            Action::CopyLink(id, kind) => self.copy_link(id, kind, cx),
            Action::ToggleLink(id) => self.toggle_link(id),
            Action::SetBrowserTrace(enabled) => self.set_browser_trace(enabled),
            Action::SetSenderTrace(enabled) => self.set_sender_trace(enabled, cx),
            Action::OpenLogs => self.open_logs(cx),
            Action::EnlargeQr(id) => self.enlarge_qr(id, window, cx),
            Action::StopShare(id) => self.stop_share(id, cx),
            Action::ShareAgain(id) => self.share_again(id, window, cx),
            Action::ConfigureShare(id) => self.configure_share(id, window, cx),
            Action::OpenAccount => self.open_account(window, cx),
            Action::ReloadAccountSelection => self.reload_account_selection(cx),
            Action::DismissCommunityNotice => {
                if self.community_notice.dismiss() {
                    tracing::info!(
                        event = "desktop.community_notice_dismissed",
                        session = std::process::id(),
                    );
                }
            }
            Action::ChooseShareRelays(id) => self.open_sharing_setup(Some(id), window, cx),
            Action::SettingsSection(section) => self.select_settings_section(section, window, cx),
            Action::ResumeShare(id) => self.resume_native_share(id, cx),
            Action::UseSavedSettings(id) => self.use_saved_settings(id, cx),
            Action::ToggleTransfer(index) => self.toggle_preview_transfer(index),
            Action::SetNotifications(choice) => {
                crate::preferences::select_notifications(choice, cx);
            }
            Action::TogglePreviewTransferDetails => {
                self.preview_transfer_details = !self.preview_transfer_details;
            }
            Action::SetAppearance(choice) => crate::preferences::select_appearance(choice, cx),
            Action::SetShareAnimation(choice) => {
                crate::preferences::select_share_animation(choice, cx);
            }
            Action::SetLanguage(choice) => {
                cx.update_global::<LanguageSettings, _>(|settings, _| settings.select(choice));
            }
            Action::Toast(message) => self.notify(message, cx),
        }
        cx.notify();
    }

    fn toggle_preview_transfer(&mut self, index: usize) {
        if let Some(transfer) = self.transfers.get_mut(index) {
            transfer.state = match transfer.state {
                TransferState::Running => TransferState::Paused,
                TransferState::Paused => TransferState::Running,
                TransferState::Complete => TransferState::Complete,
            };
        }
    }

    fn stop_share(&mut self, id: usize, cx: &gpui::App) {
        if self.runtime.is_preview() {
            if let Some(share) = self.shares.iter_mut().find(|share| share.id == id) {
                share.state = match share.state {
                    ShareState::Preparing { .. } => ShareState::Cancelled { error: None },
                    ShareState::Live { .. } => ShareState::Stopped { error: None },
                    _ => return,
                };
            }
        } else {
            self.stop_native_share(id, cx);
        }
    }

    fn share_again(&mut self, id: usize, window: &mut Window, cx: &mut Context<Self>) {
        if self.runtime.is_preview() {
            if let Some(share) = self.shares.iter_mut().find(|share| share.id == id) {
                share.state = ShareState::Live {
                    publication: SharePublication::preview(id),
                    relay_status: None,
                };
            }
        } else {
            self.repeat_native_share(id, window, cx);
        }
    }

    fn reset_share(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.share_reveal_wakeup = None;
        if let ShareFlow::Running(run) = self.share_flow
            && self.shares.iter().any(|share| {
                share.id == run.share_id
                    && matches!(
                        share.state,
                        ShareState::Preparing { .. }
                            | ShareState::AwaitingAuthorization(_)
                            | ShareState::AwaitingConfiguration { .. }
                            | ShareState::AwaitingNetwork { .. }
                    )
            })
        {
            self.stop_share(run.share_id, cx);
        }
        self.share_flow = ShareFlow::Choose;
        self.focus_task(window, cx);
    }

    fn copy_link(&mut self, id: usize, kind: LinkKind, cx: &mut Context<Self>) {
        let Some(share) = self.shares.iter().find(|share| share.id == id) else {
            return;
        };
        let Some(publication) = share.live_publication() else {
            return;
        };
        let browser_trace = self.browser_trace;
        let Some(url) = publication.url(kind, browser_trace) else {
            return;
        };
        let traced = match kind {
            LinkKind::Primary => publication.is_traced(browser_trace),
            LinkKind::Backup => publication.is_backup_traced(browser_trace),
        };
        cx.write_to_clipboard(ClipboardItem::new_string(url.to_string()));
        let revision = self.next_operation();
        self.copy_feedback = CopyFeedback::Copied {
            target: CopyTarget::ShareLink { share_id: id, kind },
            revision,
        };
        let overlay = self.overlays.last().map(|frame| &frame.content);
        let on_share_surface = match overlay {
            Some(Overlay::Detail(share_id)) => *share_id == id,
            None => {
                self.page == Page::Share
                    && matches!(self.share_flow, ShareFlow::Running(run) if run.share_id == id)
            }
            _ => false,
        };
        let inline_feedback = match kind {
            LinkKind::Primary => on_share_surface,
            LinkKind::Backup => on_share_surface && self.link_disclosure.expanded(id),
        };
        // The result button acknowledges its own action; other surfaces still
        // need a toast because they have no inline confirmation.
        if !inline_feedback {
            self.notify(
                match (kind, traced) {
                    (LinkKind::Primary, false) => Text::LinkCopied,
                    (LinkKind::Primary, true) => Text::DiagnosticLinkCopied,
                    (LinkKind::Backup, false) => Text::BackupLinkCopied,
                    (LinkKind::Backup, true) => Text::DiagnosticBackupLinkCopied,
                },
                cx,
            );
        }
        tracing::info!(
            event = "share.link_copied",
            session = std::process::id(),
            operation = revision,
            share_id = id,
            link_kind = ?kind,
            browser_trace = traced,
            inline_feedback
        );
        Self::schedule_copy_feedback_reset(revision, cx);
    }

    pub(crate) fn schedule_copy_feedback_reset(revision: usize, cx: &Context<Self>) {
        cx.spawn(async move |view, cx| {
            cx.background_executor()
                .timer(Duration::from_millis(COPY_FEEDBACK_MS))
                .await;
            let _ = view.update(cx, |this, cx| {
                if matches!(this.copy_feedback, CopyFeedback::Copied { revision: current, .. } if current == revision) {
                    this.copy_feedback = CopyFeedback::Idle;
                    cx.notify();
                }
            });
        })
        .detach();
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
