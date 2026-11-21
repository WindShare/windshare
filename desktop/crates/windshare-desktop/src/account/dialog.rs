use super::{
    AccountFormError, AccountPage, AccountReconcile, AccountScreen, AccountTracker, EmailPurpose,
    EmailStep,
};
use crate::localization::{LanguageSettings, Text, language};
use gpui::{
    AppContext, Context, Entity, EventEmitter, FocusHandle, Focusable, Subscription, Window,
};
use windshare_host::{AccountCommand, AccountState, AccountView};
use windshare_ui::{Input, InputEvent};

#[derive(Clone)]
pub enum PreviewAccountEdit {
    Nickname(String),
    Credentials(String),
    Login(String),
    ProfileEmail(String),
}

pub enum AccountSubmission {
    Register,
    Login {
        username: String,
        password: String,
    },
    Password {
        account_id: String,
        username: String,
        password: String,
    },
    Nickname {
        account_id: String,
        nickname: String,
    },
    Email {
        email: String,
    },
    Verify {
        code: String,
    },
    RequestProfileEmail {
        account_id: String,
        email: String,
    },
    ConfirmProfileEmail {
        account_id: String,
        challenge: String,
        code: String,
    },
}

impl AccountSubmission {
    pub fn to_command(&self) -> AccountCommand {
        match self {
            Self::Register => AccountCommand::Register,
            Self::Login { username, password } => AccountCommand::Login {
                username: username.clone(),
                password: password.clone(),
            },
            Self::Password {
                account_id,
                username,
                password,
            } => AccountCommand::Password {
                account_id: account_id.clone(),
                username: username.clone(),
                password: password.clone(),
            },
            Self::Nickname {
                account_id,
                nickname,
            } => AccountCommand::Nickname {
                account_id: account_id.clone(),
                nickname: nickname.clone(),
            },
            Self::Email { email } => AccountCommand::Email {
                email: email.clone(),
            },
            Self::Verify { code } => AccountCommand::Verify { code: code.clone() },
            Self::RequestProfileEmail { account_id, email } => {
                AccountCommand::RequestProfileEmail {
                    account_id: account_id.clone(),
                    email: email.clone(),
                }
            }
            Self::ConfirmProfileEmail {
                account_id,
                challenge,
                code,
            } => AccountCommand::ConfirmProfileEmail {
                account_id: account_id.clone(),
                challenge: challenge.clone(),
                code: code.clone(),
            },
        }
    }
}

pub enum AccountDialogEvent {
    Submit(AccountSubmission),
    PreviewEdit(PreviewAccountEdit),
    CopyUid,
    Close,
    PageChanged,
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum AccountOrigin {
    Settings,
    Standalone,
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum DialogRuntime {
    Native,
    Preview,
}

pub struct AccountDialog {
    pub(super) nickname: Entity<Input>,
    pub(super) username: Entity<Input>,
    pub(super) password: Entity<Input>,
    pub(super) email: Entity<Input>,
    pub(super) code: Entity<Input>,
    pub(super) page: AccountScreen<String>,
    pub(super) view: AccountView,
    pub(super) origin: AccountOrigin,
    runtime: DialogRuntime,
    pub(super) pending: bool,
    pub(super) feedback: Option<Result<Text, AccountFormError>>,
    pub(super) uid_copied: bool,
    overlay_scope: FocusHandle,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<AccountDialogEvent> for AccountDialog {}

impl AccountDialog {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        initial_page: AccountPage,
        view: AccountView,
        origin: AccountOrigin,
        is_preview: bool,
        overlay_scope: FocusHandle,
        tracker: &AccountTracker,
        window: &Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let locale = language(cx);
        let nickname = cx.new(|cx| Input::new(locale.text(Text::Nickname), cx));
        let username = cx.new(|cx| Input::new(locale.text(Text::AccountUsername), cx));
        let password =
            cx.new(|cx| Input::new(locale.text(Text::AccountPasswordLabel), cx).password());
        let email = cx.new(|cx| Input::new(locale.text(Text::AccountEmailLabel), cx));
        let code = cx.new(|cx| Input::new(locale.text(Text::AccountCode), cx));
        let mut subscriptions =
            vec![
                cx.observe_global_in::<LanguageSettings>(window, |this, _, cx| {
                    this.localize(cx);
                    cx.notify();
                }),
            ];
        for input in [&nickname, &username, &password, &email, &code] {
            subscriptions.push(
                cx.subscribe_in(input, window, |this, _, event, window, cx| match event {
                    InputEvent::Submit => this.submit(window, cx),
                    InputEvent::Changed => cx.notify(),
                }),
            );
        }
        let mut dialog = Self {
            nickname,
            username,
            password,
            email,
            code,
            page: AccountScreen::Overview,
            view,
            origin,
            runtime: if is_preview {
                DialogRuntime::Preview
            } else {
                DialogRuntime::Native
            },
            pending: tracker.pending.is_some(),
            feedback: tracker.feedback.clone(),
            uid_copied: false,
            overlay_scope,
            _subscriptions: subscriptions,
        };
        // Defer window focus to Desktop::open_scoped_overlay so it captures the
        // pre-dialog return focus before moving focus into this modal.
        dialog.apply_page(initial_page, cx);
        dialog
    }

    fn localize(&self, cx: &mut Context<Self>) {
        let locale = language(cx);
        for (input, text) in [
            (&self.nickname, Text::Nickname),
            (&self.username, Text::AccountUsername),
            (&self.password, Text::AccountPasswordLabel),
            (&self.email, Text::AccountEmailLabel),
            (&self.code, Text::AccountCode),
        ] {
            input.update(cx, |input, cx| input.set_labels(locale.text(text), "", cx));
        }
    }

    pub fn page(&self) -> AccountPage {
        self.page.map_target(|_| ())
    }

    pub fn title(&self) -> Text {
        self.page().title(&self.view)
    }

    pub const fn busy(&self) -> bool {
        self.pending || self.view.busy()
    }

    pub const fn can_register(&self) -> bool {
        !self.busy()
            && self.view.status.account.is_empty()
            && !matches!(
                self.view.status.state,
                AccountState::Registering { .. }
                    | AccountState::Denied
                    | AccountState::StorageError
            )
    }

    fn target_changed(&self, view: &AccountView) -> bool {
        self.page
            .target()
            .is_some_and(|target| target != &view.status.account)
    }

    pub fn field_focus(&self, cx: &gpui::App) -> Option<FocusHandle> {
        let input = match self.page {
            AccountScreen::Login | AccountScreen::Credentials(_) => &self.username,
            AccountScreen::Nickname(_) => &self.nickname,
            AccountScreen::Email(_, EmailStep::Code) => &self.code,
            AccountScreen::Email(_, EmailStep::Address) => &self.email,
            AccountScreen::Overview | AccountScreen::Registration => return None,
        };
        Some(input.read(cx).focus_handle(cx))
    }

    pub fn focus_target(&self, cx: &gpui::App) -> FocusHandle {
        self.field_focus(cx)
            .unwrap_or_else(|| self.overlay_scope.clone())
    }

    pub(super) fn can_submit(&self, cx: &Context<Self>) -> bool {
        let view = &self.view;
        if self.busy() || self.target_changed(view) {
            return false;
        }
        match self.page() {
            AccountPage::Overview => false,
            AccountPage::Registration => self.can_register(),
            AccountPage::Login | AccountPage::Credentials(()) => {
                !self.username.read(cx).value().trim().is_empty()
                    && !self.password.read(cx).value().is_empty()
            }
            AccountPage::Nickname(()) => {
                let value = self.nickname.read(cx).value().trim();
                view.can_edit_profile()
                    && view.nickname_locked_until().is_none()
                    && !value.is_empty()
                    && view
                        .status
                        .profile
                        .nickname
                        .as_ref()
                        .is_none_or(|current| current.value != value)
            }
            AccountPage::Email(purpose, step) => self.can_submit_email(purpose, step, cx),
        }
    }

    fn can_submit_email(&self, purpose: EmailPurpose, step: EmailStep, cx: &Context<Self>) -> bool {
        let view = &self.view;
        let available = match purpose {
            EmailPurpose::Registration => view.status.state == AccountState::EmailRequired,
            EmailPurpose::Profile(()) => view.can_edit_profile(),
        };
        if !available {
            return false;
        }
        match step {
            EmailStep::Code => {
                purpose.has_code(view) && !self.code.read(cx).value().trim().is_empty()
            }
            EmailStep::Address => {
                let address = self.email.read(cx).value().trim();
                !address.is_empty()
                    && (purpose == EmailPurpose::Registration
                        || !address.eq_ignore_ascii_case(&view.status.profile.email))
            }
        }
    }

    pub(super) fn open_page(
        &mut self,
        page: AccountPage,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.busy() || !self.apply_page(page, cx) {
            return;
        }
        let focus = self.focus_target(cx);
        window.focus(&focus, cx);
        cx.emit(AccountDialogEvent::PageChanged);
        cx.notify();
    }

    fn apply_page(&mut self, page: AccountPage, cx: &mut Context<Self>) -> bool {
        if page.target().is_some()
            && (self.view.status.account.is_empty() || self.target_changed(&self.view))
        {
            return false;
        }
        let username = if page == AccountPage::Credentials(()) {
            self.view.status.username.clone()
        } else {
            String::new()
        };
        let nickname = self
            .view
            .status
            .profile
            .nickname
            .as_ref()
            .map_or_default(|nickname| nickname.value.clone());
        let email = match page {
            AccountPage::Email(EmailPurpose::Profile(()), EmailStep::Address) => Some(
                self.view
                    .status
                    .pending_email
                    .as_ref()
                    .map_or(&self.view.status.profile.email, |pending| &pending.address)
                    .clone(),
            ),
            AccountPage::Email(EmailPurpose::Registration, EmailStep::Address)
                if !matches!(
                    self.page(),
                    AccountPage::Email(EmailPurpose::Registration, _)
                ) =>
            {
                Some(String::new())
            }
            _ => None,
        };
        self.page = page.map_target(|()| self.view.status.account.clone());
        if page == AccountPage::Nickname(()) {
            self.nickname
                .update(cx, |input, cx| input.set_value(nickname, cx));
        }
        if let Some(email) = email {
            self.email
                .update(cx, |input, cx| input.set_value(email, cx));
        }
        self.feedback = None;
        self.clear_secrets(cx);
        self.username
            .update(cx, |input, cx| input.set_value(username, cx));
        true
    }

    pub(super) fn register(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if !self.can_register() {
            return;
        }
        self.page = AccountScreen::Registration;
        self.feedback = None;
        cx.emit(AccountDialogEvent::Submit(AccountSubmission::Register));
        window.focus(&self.overlay_scope, cx);
        cx.notify();
    }

    pub(super) fn submit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if !self.can_submit(cx) {
            return;
        }
        if self.runtime == DialogRuntime::Preview {
            self.submit_preview(cx);
            return;
        }
        let Some(submission) = self.build_submission(cx) else {
            return;
        };
        let registering = self.page() == AccountPage::Registration;
        self.feedback = None;
        cx.emit(AccountDialogEvent::Submit(submission));
        // The primary button leaves the tree while the account is being created.
        if registering {
            window.focus(&self.overlay_scope, cx);
        }
        cx.notify();
    }

    fn build_submission(&self, cx: &Context<Self>) -> Option<AccountSubmission> {
        match &self.page {
            AccountScreen::Overview => None,
            AccountScreen::Registration => Some(AccountSubmission::Register),
            AccountScreen::Login | AccountScreen::Credentials(_) => {
                let username = self.username.read(cx).value().trim().to_owned();
                let password = self.password.read(cx).value().to_owned();
                Some(if let AccountScreen::Credentials(account_id) = &self.page {
                    AccountSubmission::Password {
                        account_id: account_id.clone(),
                        username,
                        password,
                    }
                } else {
                    AccountSubmission::Login { username, password }
                })
            }
            AccountScreen::Nickname(account_id) => Some(AccountSubmission::Nickname {
                account_id: account_id.clone(),
                nickname: self.nickname.read(cx).value().trim().into(),
            }),
            AccountScreen::Email(EmailPurpose::Registration, EmailStep::Code) => {
                Some(AccountSubmission::Verify {
                    code: self.code.read(cx).value().trim().into(),
                })
            }
            AccountScreen::Email(EmailPurpose::Registration, EmailStep::Address) => {
                Some(AccountSubmission::Email {
                    email: self.email.read(cx).value().trim().into(),
                })
            }
            AccountScreen::Email(EmailPurpose::Profile(account_id), EmailStep::Address) => {
                Some(AccountSubmission::RequestProfileEmail {
                    account_id: account_id.clone(),
                    email: self.email.read(cx).value().trim().into(),
                })
            }
            AccountScreen::Email(EmailPurpose::Profile(account_id), EmailStep::Code) => {
                let pending = self.view.status.pending_email.as_ref()?;
                Some(AccountSubmission::ConfirmProfileEmail {
                    account_id: account_id.clone(),
                    challenge: pending.challenge.id.clone(),
                    code: self.code.read(cx).value().trim().into(),
                })
            }
        }
    }

    fn submit_preview(&mut self, cx: &mut Context<Self>) {
        let edit = match self.page() {
            AccountPage::Nickname(()) => {
                PreviewAccountEdit::Nickname(self.nickname.read(cx).value().trim().to_owned())
            }
            AccountPage::Credentials(()) => {
                PreviewAccountEdit::Credentials(self.username.read(cx).value().trim().to_owned())
            }
            AccountPage::Login => {
                PreviewAccountEdit::Login(self.username.read(cx).value().trim().to_owned())
            }
            AccountPage::Email(EmailPurpose::Profile(()), EmailStep::Address) => {
                PreviewAccountEdit::ProfileEmail(self.email.read(cx).value().trim().to_owned())
            }
            _ => return,
        };
        self.clear_secrets(cx);
        self.page = AccountScreen::Overview;
        cx.emit(AccountDialogEvent::PreviewEdit(edit));
    }

    pub fn sync_tracker(&mut self, tracker: &AccountTracker, cx: &mut Context<Self>) {
        self.pending = tracker.pending.is_some();
        self.feedback.clone_from(&tracker.feedback);
        cx.notify();
    }

    pub fn set_uid_copied(&mut self, copied: bool, cx: &mut Context<Self>) {
        if self.uid_copied != copied {
            self.uid_copied = copied;
            cx.notify();
        }
    }

    pub fn sync_model(
        &mut self,
        view: &AccountView,
        tracker: &mut AccountTracker,
        outcome: &AccountReconcile,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> AccountPage {
        let previous_page = self.page();
        let previous_field = self.field_focus(cx);
        self.view = view.clone();
        if self.target_changed(view) {
            self.close_stale_edit(tracker, cx);
        } else {
            match outcome {
                AccountReconcile::Failed(AccountFormError::AccountChanged)
                    if self.page.target().is_some() =>
                {
                    self.close_stale_edit(tracker, cx);
                }
                AccountReconcile::Succeeded => self.completed(view, tracker, cx),
                AccountReconcile::Failed(_) | AccountReconcile::Idle => {}
            }
            if !self.target_changed(view) {
                self.reconcile_page(view, tracker, cx);
            }
        }
        self.pending = tracker.pending.is_some();
        self.feedback.clone_from(&tracker.feedback);
        let next_field = self.field_focus(cx);
        if previous_field != next_field {
            let focus = self.focus_target(cx);
            window.focus(&focus, cx);
        }
        cx.notify();
        previous_page
    }

    fn reconcile_page(
        &mut self,
        view: &AccountView,
        tracker: &mut AccountTracker,
        cx: &mut Context<Self>,
    ) {
        match &mut self.page {
            AccountScreen::Registration | AccountScreen::Email(EmailPurpose::Registration, _)
                if !view.status.account.is_empty() =>
            {
                self.page = AccountScreen::Overview;
                tracker.feedback = None;
                self.clear_secrets(cx);
            }
            AccountScreen::Registration if view.status.state == AccountState::EmailRequired => {
                self.page = EmailPurpose::Registration.entry(view);
            }
            AccountScreen::Email(EmailPurpose::Registration, _)
                if view.status.state != AccountState::EmailRequired =>
            {
                self.page = AccountScreen::entry(view).0;
            }
            AccountScreen::Nickname(_) | AccountScreen::Email(EmailPurpose::Profile(_), _)
                if !view.can_edit_profile() =>
            {
                self.page = AccountScreen::Overview;
            }
            AccountScreen::Nickname(_) if view.nickname_locked_until().is_some() => {
                self.page = AccountScreen::Overview;
            }
            AccountScreen::Email(purpose, step @ EmailStep::Code) if !purpose.has_code(view) => {
                *step = EmailStep::Address;
            }
            _ => {}
        }
    }

    fn close_stale_edit(&mut self, tracker: &mut AccountTracker, cx: &mut Context<Self>) {
        self.page = AccountScreen::Overview;
        tracker.pending = None;
        tracker.feedback = Some(Err(AccountFormError::AccountChanged));
        self.clear_secrets(cx);
    }

    fn clear_secrets(&self, cx: &mut Context<Self>) {
        self.password
            .update(cx, |input, cx| input.set_value("", cx));
        self.code.update(cx, |input, cx| input.set_value("", cx));
    }

    fn completed(
        &mut self,
        view: &AccountView,
        tracker: &mut AccountTracker,
        cx: &mut Context<Self>,
    ) {
        let message = match &mut self.page {
            AccountScreen::Registration if view.status.state == AccountState::EmailRequired => {
                self.page = EmailPurpose::Registration.entry(view);
                return;
            }
            AccountScreen::Registration => Text::AccountCreated,
            AccountScreen::Login => Text::AccountSignedIn,
            AccountScreen::Credentials(_) => Text::AccountLoginSaved,
            AccountScreen::Nickname(_) => Text::NicknameSaved,
            AccountScreen::Email(purpose, step @ EmailStep::Address) => {
                *step = purpose.step(view);
                self.code.update(cx, |input, cx| input.set_value("", cx));
                return;
            }
            AccountScreen::Email(EmailPurpose::Registration, EmailStep::Code) => {
                Text::AccountVerified
            }
            AccountScreen::Email(EmailPurpose::Profile(_), EmailStep::Code) => {
                Text::ProfileEmailSaved
            }
            AccountScreen::Overview => return,
        };
        self.clear_secrets(cx);
        self.page = AccountScreen::Overview;
        tracker.feedback = Some(Ok(message));
    }
}
