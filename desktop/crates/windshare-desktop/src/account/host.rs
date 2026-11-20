use super::{AccountDialog, AccountDialogEvent, AccountOrigin, AccountPage, PreviewAccountEdit};
use crate::{
    desktop::Desktop,
    flow_state::{CopyFeedback, CopyTarget},
    live::Runtime,
    localization::Text,
    model::{Overlay, Page, SettingsSection},
};
use gpui::{App, ClipboardItem, Context, Window, prelude::*};
use windshare_host::{AccountCommand, AccountView, Failure};

impl Desktop {
    pub(crate) fn account_view(&self) -> Option<&AccountView> {
        match &self.runtime {
            Runtime::Native(session) => session.model.account.as_ref(),
            Runtime::Preview(preview) => Some(&preview.account),
        }
    }

    pub fn open_account(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if let Runtime::Native(session) = &self.runtime {
            session.controller.reload_account_selection();
        }
        let Some(page) = self.account_view().map(|view| AccountPage::entry(view).0) else {
            return;
        };
        if page == AccountPage::Overview {
            self.select_settings_section(SettingsSection::Account, window, cx);
            return;
        }
        self.open_account_modal(page, window, cx);
    }

    pub fn open_account_modal(
        &mut self,
        page: AccountPage,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(view) = self.account_view().cloned() else {
            return;
        };
        if let Runtime::Native(session) = &self.runtime {
            session.controller.reload_account_selection();
        }
        let origin =
            if self.page == Page::Settings && self.settings_section == SettingsSection::Account {
                AccountOrigin::Settings
            } else {
                AccountOrigin::Standalone
            };
        let is_preview = self.runtime.is_preview();
        let scope = cx.focus_handle();
        let dialog = cx.new(|cx| {
            AccountDialog::new(
                page,
                view,
                origin,
                is_preview,
                scope.clone(),
                &self.account,
                window,
                cx,
            )
        });
        let subscription = cx.subscribe_in(&dialog, window, |this, _, event, window, cx| {
            this.handle_account_dialog_event(event, window, cx);
        });
        self.open_scoped_overlay(
            Overlay::Account(dialog),
            scope,
            Some(subscription),
            window,
            cx,
        );
    }

    fn handle_account_dialog_event(
        &mut self,
        event: &AccountDialogEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            AccountDialogEvent::Submit(submission) => {
                self.send_account_command(submission.to_command(), cx);
            }
            AccountDialogEvent::PreviewEdit(edit) => {
                self.apply_preview_account_edit(edit.clone(), window, cx);
            }
            AccountDialogEvent::CopyUid => self.copy_account_uid(cx),
            AccountDialogEvent::Close => self.close_overlay(window, cx),
            AccountDialogEvent::PageChanged => {}
        }
        cx.notify();
    }

    pub(crate) fn register_account(&mut self, cx: &mut Context<Self>) {
        if let Some(view) = self.account_view()
            && self.account.can_register(view)
        {
            self.send_account_command(AccountCommand::Register, cx);
            cx.notify();
        }
    }

    pub(crate) fn refresh_account(&mut self, cx: &mut Context<Self>) {
        if let Some(view) = self.account_view()
            && !self.account.busy(view)
        {
            self.send_account_command(AccountCommand::Refresh, cx);
            cx.notify();
        }
    }

    fn send_account_command(&mut self, command: AccountCommand, cx: &mut Context<Self>) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        self.account.pending = session.controller.account(command);
        self.account.feedback = if self.account.pending.is_some() {
            None
        } else {
            session
                .controller
                .model()
                .command_error
                .clone()
                .map(|error| Err(Failure::from(error).into()))
        };
        if let Some(Overlay::Account(dialog)) = self.overlays.last().map(|frame| &frame.content) {
            dialog.update(cx, |dialog, cx| dialog.sync_tracker(&self.account, cx));
        }
    }

    fn apply_preview_account_edit(
        &mut self,
        edit: PreviewAccountEdit,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Runtime::Preview(preview) = &mut self.runtime else {
            return;
        };
        let toast = match edit {
            PreviewAccountEdit::Nickname(nickname) => {
                preview.account.status.profile.nickname = Some(windshare_host::Nickname {
                    value: nickname,
                    next_change_at: 0,
                });
                Text::NicknameSaved
            }
            PreviewAccountEdit::Credentials(username) => {
                preview.account.status.username = username;
                Text::AccountLoginSaved
            }
            PreviewAccountEdit::Login(username) => {
                preview.account.status.username = username;
                Text::AccountSignedIn
            }
            PreviewAccountEdit::ProfileEmail(email) => {
                preview.account.status.profile.email = email;
                Text::ProfileEmailSaved
            }
        };
        self.close_overlay(window, cx);
        self.notify(toast, cx);
    }

    pub(crate) fn copy_account_uid(&mut self, cx: &mut Context<Self>) {
        let Some(uid) = self
            .account_view()
            .map(|view| view.status.account.clone())
            .filter(|uid| !uid.is_empty())
        else {
            return;
        };
        cx.write_to_clipboard(ClipboardItem::new_string(uid));
        let revision = self.next_operation();
        self.copy_feedback = CopyFeedback::Copied {
            target: CopyTarget::AccountUid,
            revision,
        };
        self.sync_account_dialog_uid_copied(true, cx);
        tracing::info!(
            event = "account.uid_copied",
            session = std::process::id(),
            operation = revision,
        );
        Self::schedule_copy_feedback_reset(revision, cx);
    }

    pub(crate) fn sync_account_dialog_uid_copied(&self, copied: bool, cx: &mut Context<Self>) {
        if let Some(Overlay::Account(dialog)) = self.overlays.last().map(|frame| &frame.content) {
            dialog.update(cx, |dialog, cx| dialog.set_uid_copied(copied, cx));
        }
    }

    pub(crate) fn sync_account_view_visibility(&self, window: &Window, cx: &App) {
        if let Runtime::Native(session) = &self.runtime {
            let viewing_page = self.page == Page::Settings
                && self.settings_section == SettingsSection::Account
                && self.overlays.is_empty();
            let viewing_overlay = self.overlays.last().is_some_and(|frame| {
                matches!(
                    &frame.content,
                    Overlay::Account(dialog) if dialog.read(cx).page() == AccountPage::Overview
                )
            });
            session.controller.set_account_view_visible(
                window.is_visible()
                    && (viewing_page || viewing_overlay)
                    && self
                        .account_view()
                        .is_some_and(|view| !view.status.account.is_empty()),
            );
        }
    }
}
