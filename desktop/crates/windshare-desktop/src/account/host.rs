use super::{
    AccountDialog, AccountDialogEvent, AccountFormError, AccountOrigin, AccountPage, AccountSource,
    PreviewAccountEdit, can_register_account,
};
use crate::{
    desktop::Desktop,
    flow_state::{CopyFeedback, CopyTarget},
    live::Runtime,
    localization::Text,
    model::{Overlay, Page, SettingsSection},
};
use gpui::{App, ClipboardItem, Context, Window, prelude::*};
use windshare_host::{AccountCommand, AccountView};

impl Desktop {
    pub(crate) fn account_view<'a>(&'a self, cx: &'a App) -> Option<&'a AccountView> {
        match &self.runtime {
            Runtime::Native(session) => session.model(cx).account.as_ref(),
            Runtime::Preview(preview) => Some(&preview.account),
        }
    }

    pub(super) fn account_busy(&self, cx: &App) -> bool {
        match &self.runtime {
            Runtime::Native(session) => session.model(cx).account_busy(),
            Runtime::Preview(preview) => preview.account.busy(),
        }
    }

    pub(super) fn account_inline_feedback(
        &self,
        cx: &App,
    ) -> Option<Result<Text, AccountFormError>> {
        let Runtime::Native(session) = &self.runtime else {
            return None;
        };
        self.account_ui.inline_feedback(session.model(cx))
    }

    pub fn open_account(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if let Runtime::Native(session) = &self.runtime {
            session.controller(cx).reload_account_selection();
        }
        let Some(page) = self.account_view(cx).map(|view| AccountPage::entry(view).0) else {
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
        let source = match &self.runtime {
            Runtime::Native(session) => {
                if session.model(cx).account.is_none() {
                    return;
                }
                session.controller(cx).reload_account_selection();
                AccountSource::Native(session.host.clone())
            }
            Runtime::Preview(preview) => AccountSource::Preview(Box::new(preview.account.clone())),
        };
        self.account_ui.clear();
        let origin =
            if self.page == Page::Settings && self.settings_section == SettingsSection::Account {
                AccountOrigin::Settings
            } else {
                AccountOrigin::Standalone
            };
        let scope = cx.focus_handle();
        let dialog =
            cx.new(|cx| AccountDialog::new(page, source, origin, scope.clone(), window, cx));
        let initial_focus = dialog.read(cx).focus_target(cx);
        let subscription = cx.subscribe_in(&dialog, window, |this, _, event, window, cx| {
            this.handle_account_dialog_event(event, window, cx);
        });
        self.open_scoped_overlay(
            Overlay::Account(dialog),
            scope,
            &initial_focus,
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
            AccountDialogEvent::Completed(toast) => {
                self.close_overlay(window, cx);
                if let Some(message) = toast {
                    self.notify(*message, cx);
                }
            }
            AccountDialogEvent::PreviewEdit(edit) => {
                self.apply_preview_account_edit(edit.clone(), window, cx);
            }
            AccountDialogEvent::Close => self.close_overlay(window, cx),
            AccountDialogEvent::PageChanged => {}
        }
        cx.notify();
    }

    pub(crate) fn register_account(&mut self, cx: &mut Context<Self>) {
        if let Some(view) = self.account_view(cx)
            && can_register_account(view, self.account_busy(cx))
            && let Runtime::Native(session) = &self.runtime
        {
            let host = session.host.clone();
            self.account_ui.inline_request_id = host.update(cx, |host, cx| {
                host.submit_account(AccountCommand::Register, cx)
            });
            cx.notify();
        }
    }

    pub(crate) fn reload_account_selection(&self, cx: &App) {
        if let Runtime::Native(session) = &self.runtime {
            session.controller(cx).reload_account_selection();
        }
    }

    pub(crate) fn refresh_account(&mut self, cx: &mut Context<Self>) {
        if self.account_view(cx).is_some()
            && !self.account_busy(cx)
            && let Runtime::Native(session) = &self.runtime
        {
            let host = session.host.clone();
            self.account_ui.inline_request_id = host.update(cx, |host, cx| {
                host.submit_account(AccountCommand::Refresh, cx)
            });
            cx.notify();
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

    pub(crate) fn copy_account_uid(&mut self, cx: &Context<Self>) {
        let Some(uid) = self
            .account_view(cx)
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
        tracing::info!(
            event = "account.uid_copied",
            session = std::process::id(),
            operation = revision,
        );
        Self::schedule_copy_feedback_reset(revision, cx);
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
            session.controller(cx).set_account_view_visible(
                window.is_visible()
                    && (viewing_page || viewing_overlay)
                    && self
                        .account_view(cx)
                        .is_some_and(|view| !view.status.account.is_empty()),
            );
        }
    }
}
