use super::{
    AccountFormError, AccountPage, AccountScreen, EmailPurpose, EmailStep, can_register_account,
};
use crate::{
    flow_state::COPY_FEEDBACK_MS,
    live::HostStore,
    localization::{LanguageSettings, Text, language},
};
use gpui::{
    App, AppContext, ClipboardItem, Context, Entity, EventEmitter, FocusHandle, Focusable,
    Subscription, Window,
};
use std::{sync::Arc, time::Duration};
use windshare_host::{AccountCommand, AccountRequest, AccountState, AccountView};
use windshare_ui::{Input, InputEvent};

#[derive(Clone)]
pub enum PreviewAccountEdit {
    Nickname(String),
    Credentials(String),
    Login(String),
    ProfileEmail(String),
}

pub enum AccountDialogEvent {
    Completed(Option<Text>),
    PreviewEdit(PreviewAccountEdit),
    Close,
    PageChanged,
    FocusChanged,
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum AccountOrigin {
    Settings,
    Standalone,
}

pub enum AccountSource {
    Native(Entity<HostStore>),
    Preview(Box<AccountView>),
}

pub struct AccountDialog {
    pub(super) nickname: Entity<Input>,
    pub(super) username: Entity<Input>,
    pub(super) password: Entity<Input>,
    pub(super) email: Entity<Input>,
    pub(super) code: Entity<Input>,
    pub(super) page: AccountScreen<String>,
    pub(super) origin: AccountOrigin,
    source: AccountSource,
    in_flight_id: Option<String>,
    pub(super) feedback: Option<Result<Text, AccountFormError>>,
    pub(super) uid_copied: bool,
    copy_sequence: usize,
    overlay_scope: FocusHandle,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<AccountDialogEvent> for AccountDialog {}

impl AccountDialog {
    pub fn new(
        initial_page: AccountPage,
        source: AccountSource,
        origin: AccountOrigin,
        initial_error: Option<AccountFormError>,
        overlay_scope: FocusHandle,
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
            subscriptions.push(cx.subscribe_in(
                input,
                window,
                |this, _, event, _, cx| match event {
                    InputEvent::Submit => this.submit(cx),
                    InputEvent::Changed => cx.notify(),
                },
            ));
        }
        let in_flight_id = match &source {
            AccountSource::Native(host) => {
                subscriptions.push(cx.observe(host, |this, host, cx| {
                    this.on_host_updated(&host, cx);
                }));
                match &host.read(cx).model().account_request {
                    AccountRequest::Pending { id, action }
                        if initial_page.matches_pending_action(*action) =>
                    {
                        Some(id.clone())
                    }
                    _ => None,
                }
            }
            AccountSource::Preview(_) => None,
        };
        let mut dialog = Self {
            nickname,
            username,
            password,
            email,
            code,
            page: AccountScreen::Overview,
            origin,
            source,
            in_flight_id,
            feedback: None,
            uid_copied: false,
            copy_sequence: 0,
            overlay_scope,
            _subscriptions: subscriptions,
        };
        // Defer window focus to Desktop::open_scoped_overlay so it captures the
        // pre-dialog return focus before moving focus into this modal.
        if dialog.apply_page(initial_page, cx) {
            // Attach any unseen background failure after apply_page resets form state.
            dialog.feedback = initial_error.map(Err);
        }
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

    pub(super) fn view<'a>(&'a self, cx: &'a App) -> Option<&'a AccountView> {
        match &self.source {
            AccountSource::Native(host) => host.read(cx).model().account.as_ref(),
            AccountSource::Preview(view) => Some(view),
        }
    }

    pub fn page(&self) -> AccountPage {
        self.page.map_target(|_| ())
    }

    pub fn title(&self, cx: &App) -> Text {
        self.view(cx)
            .map_or(Text::Account, |view| self.page().title(view))
    }

    pub const fn has_error_feedback(&self) -> bool {
        matches!(self.feedback, Some(Err(_)))
    }

    pub fn will_complete_on(&self, model: &windshare_host::Model) -> bool {
        self.page() != AccountPage::Overview
            && self.in_flight_id.as_deref().is_some_and(|tracked| {
                matches!(&model.account_request, AccountRequest::Succeeded { id } if id == tracked)
            })
    }

    pub fn busy(&self, cx: &App) -> bool {
        match &self.source {
            AccountSource::Native(host) => host.read(cx).model().account_busy(),
            AccountSource::Preview(view) => view.busy(),
        }
    }

    pub(super) fn registering(&self, cx: &App) -> bool {
        match &self.source {
            AccountSource::Native(host) => {
                host.read(cx).model().in_flight_registration_id().is_some()
            }
            AccountSource::Preview(view) => view.busy(),
        }
    }

    pub fn can_register(&self, cx: &App) -> bool {
        self.view(cx)
            .is_some_and(|view| can_register_account(view, self.busy(cx)))
    }

    fn target_changed(&self, view: &AccountView) -> bool {
        self.page
            .target()
            .is_some_and(|target| target != &view.status.account)
    }

    fn field_focus(&self, cx: &App) -> Option<FocusHandle> {
        let input = match self.page {
            AccountScreen::Login | AccountScreen::Credentials(_) => &self.username,
            AccountScreen::Nickname(_) => &self.nickname,
            AccountScreen::Email(_, EmailStep::Code) => &self.code,
            AccountScreen::Email(_, EmailStep::Address) => &self.email,
            AccountScreen::Overview | AccountScreen::Registration => return None,
        };
        Some(input.read(cx).focus_handle(cx))
    }

    pub fn focus_target(&self, cx: &App) -> FocusHandle {
        self.field_focus(cx)
            .unwrap_or_else(|| self.overlay_scope.clone())
    }

    pub(super) fn can_submit(&self, cx: &Context<Self>) -> bool {
        let Some(view) = self.view(cx) else {
            return false;
        };
        if self.busy(cx) || self.target_changed(view) {
            return false;
        }
        match self.page() {
            AccountPage::Overview => false,
            AccountPage::Registration => self.can_register(cx),
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
            AccountPage::Email(purpose, step) => self.can_submit_email(view, purpose, step, cx),
        }
    }

    fn can_submit_email(
        &self,
        view: &AccountView,
        purpose: EmailPurpose,
        step: EmailStep,
        cx: &Context<Self>,
    ) -> bool {
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

    pub(super) fn open_page(&mut self, page: AccountPage, cx: &mut Context<Self>) {
        if self.busy(cx) || !self.apply_page(page, cx) {
            return;
        }
        // Desktop owns overlay focus so only the active top dialog can move window focus.
        cx.emit(AccountDialogEvent::PageChanged);
        cx.notify();
    }

    fn apply_page(&mut self, page: AccountPage, cx: &mut Context<Self>) -> bool {
        let Some(view) = self.view(cx).cloned() else {
            return false;
        };
        if page.target().is_some() && (view.status.account.is_empty() || self.target_changed(&view))
        {
            return false;
        }
        let username = if page == AccountPage::Credentials(()) {
            view.status.username.clone()
        } else {
            String::new()
        };
        let nickname = view
            .status
            .profile
            .nickname
            .as_ref()
            .map_or_default(|nickname| nickname.value.clone());
        let email = match page {
            AccountPage::Email(EmailPurpose::Profile(()), EmailStep::Address) => Some(
                view.status
                    .pending_email
                    .as_ref()
                    .map_or(&view.status.profile.email, |pending| &pending.address)
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
        self.page = page.map_target(|()| view.status.account.clone());
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

    pub(super) fn register(&mut self, cx: &mut Context<Self>) {
        if !self.can_register(cx) {
            return;
        }
        self.page = AccountScreen::Registration;
        self.feedback = None;
        self.dispatch_command(AccountCommand::Register, cx);
        // The primary button leaves the tree while the account is being created.
        cx.emit(AccountDialogEvent::FocusChanged);
        cx.notify();
    }

    pub(super) fn submit(&mut self, cx: &mut Context<Self>) {
        if !self.can_submit(cx) {
            return;
        }
        if matches!(self.source, AccountSource::Preview(_)) {
            self.submit_preview(cx);
            return;
        }
        let Some(command) = self.build_command(cx) else {
            return;
        };
        let registering = self.page() == AccountPage::Registration;
        self.feedback = None;
        self.dispatch_command(command, cx);
        // The primary button leaves the tree while the account is being created.
        if registering {
            cx.emit(AccountDialogEvent::FocusChanged);
        }
        cx.notify();
    }

    fn dispatch_command(&mut self, command: AccountCommand, cx: &mut Context<Self>) {
        let AccountSource::Native(host) = &self.source else {
            return;
        };
        let host = host.clone();
        self.in_flight_id = host.update(cx, |host, cx| host.submit_account(command, cx));
    }

    fn build_command(&self, cx: &Context<Self>) -> Option<AccountCommand> {
        match &self.page {
            AccountScreen::Overview => None,
            AccountScreen::Registration => Some(AccountCommand::Register),
            AccountScreen::Login | AccountScreen::Credentials(_) => {
                let username = self.username.read(cx).value().trim().to_owned();
                let password = self.password.read(cx).value().to_owned();
                Some(if let AccountScreen::Credentials(account_id) = &self.page {
                    AccountCommand::Password {
                        account_id: account_id.clone(),
                        username,
                        password,
                    }
                } else {
                    AccountCommand::Login { username, password }
                })
            }
            AccountScreen::Nickname(account_id) => Some(AccountCommand::Nickname {
                account_id: account_id.clone(),
                nickname: self.nickname.read(cx).value().trim().into(),
            }),
            AccountScreen::Email(EmailPurpose::Registration, EmailStep::Code) => {
                Some(AccountCommand::Verify {
                    code: self.code.read(cx).value().trim().into(),
                })
            }
            AccountScreen::Email(EmailPurpose::Registration, EmailStep::Address) => {
                Some(AccountCommand::Email {
                    email: self.email.read(cx).value().trim().into(),
                })
            }
            AccountScreen::Email(EmailPurpose::Profile(account_id), EmailStep::Address) => {
                Some(AccountCommand::RequestProfileEmail {
                    account_id: account_id.clone(),
                    email: self.email.read(cx).value().trim().into(),
                })
            }
            AccountScreen::Email(EmailPurpose::Profile(account_id), EmailStep::Code) => {
                let pending = self.view(cx)?.status.pending_email.as_ref()?;
                Some(AccountCommand::ConfirmProfileEmail {
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

    pub(super) fn copy_uid(&mut self, cx: &mut Context<Self>) {
        let Some(uid) = self
            .view(cx)
            .map(|view| view.status.account.clone())
            .filter(|uid| !uid.is_empty())
        else {
            return;
        };
        cx.write_to_clipboard(ClipboardItem::new_string(uid));
        self.copy_sequence += 1;
        let sequence = self.copy_sequence;
        self.uid_copied = true;
        tracing::info!(
            event = "account.uid_copied",
            session = std::process::id(),
            operation = sequence,
        );
        cx.spawn(async move |view, cx| {
            cx.background_executor()
                .timer(Duration::from_millis(COPY_FEEDBACK_MS))
                .await;
            let _ = view.update(cx, |this, cx| {
                if this.copy_sequence == sequence {
                    this.uid_copied = false;
                    cx.notify();
                }
            });
        })
        .detach();
        cx.notify();
    }

    fn on_host_updated(&mut self, host: &Entity<HostStore>, cx: &mut Context<Self>) {
        let (previous_model, model) = {
            let store = host.read(cx);
            (
                Arc::clone(store.previous_model()),
                Arc::clone(store.model()),
            )
        };
        let Some(view) = model.account.as_ref() else {
            return;
        };
        let previous_page = self.page();
        let mut completed_toast = None;
        if self.target_changed(view) {
            self.close_stale_edit(cx);
        } else {
            if let Some(tracked) = self.in_flight_id.as_deref() {
                match &model.account_request {
                    AccountRequest::Succeeded { id } if id == tracked => {
                        self.in_flight_id = None;
                        completed_toast = self.completed(view, cx);
                    }
                    AccountRequest::Failed { id, error } if id == tracked => {
                        self.in_flight_id = None;
                        let form_error = AccountFormError::from(error.clone());
                        if matches!(form_error, AccountFormError::AccountChanged)
                            && self.page.target().is_some()
                        {
                            self.close_stale_edit(cx);
                        } else {
                            self.feedback = Some(Err(form_error));
                        }
                    }
                    _ => {}
                }
            }
            if !self.target_changed(view) {
                self.reconcile_page(view, cx);
            }
        }
        if let Some(event) =
            self.host_update_event(previous_page, &previous_model, view, completed_toast)
        {
            cx.emit(event);
        }
        cx.notify();
    }

    fn host_update_event(
        &self,
        previous_page: AccountPage,
        previous_model: &windshare_host::Model,
        view: &AccountView,
        completed_toast: Option<Text>,
    ) -> Option<AccountDialogEvent> {
        if previous_page != AccountPage::Overview && self.page() == AccountPage::Overview {
            if let Some(message) = completed_toast {
                return Some(AccountDialogEvent::Completed(Some(message)));
            }
            let had_no_account = previous_model
                .account
                .as_ref()
                .is_some_and(|prev| prev.status.account.is_empty());
            if self.feedback.is_none() && had_no_account && !view.status.account.is_empty() {
                return Some(AccountDialogEvent::Completed(None));
            }
        }
        (self.page() != previous_page).then_some(AccountDialogEvent::PageChanged)
    }

    fn reconcile_page(&mut self, view: &AccountView, cx: &mut Context<Self>) {
        match &mut self.page {
            AccountScreen::Registration | AccountScreen::Email(EmailPurpose::Registration, _)
                if !view.status.account.is_empty() =>
            {
                self.page = AccountScreen::Overview;
                self.feedback = None;
                self.clear_secrets(cx);
            }
            AccountScreen::Registration if view.status.state == AccountState::EmailRequired => {
                self.page = EmailPurpose::Registration.entry(view);
                self.feedback = None;
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

    fn close_stale_edit(&mut self, cx: &mut Context<Self>) {
        self.page = AccountScreen::Overview;
        self.in_flight_id = None;
        self.feedback = Some(Err(AccountFormError::AccountChanged));
        self.clear_secrets(cx);
    }

    fn clear_secrets(&self, cx: &mut Context<Self>) {
        self.password
            .update(cx, |input, cx| input.set_value("", cx));
        self.code.update(cx, |input, cx| input.set_value("", cx));
    }

    fn completed(&mut self, view: &AccountView, cx: &mut Context<Self>) -> Option<Text> {
        let message = match &mut self.page {
            AccountScreen::Registration if view.status.state == AccountState::EmailRequired => {
                self.page = EmailPurpose::Registration.entry(view);
                return None;
            }
            AccountScreen::Registration => Text::AccountCreated,
            AccountScreen::Login => Text::AccountSignedIn,
            AccountScreen::Credentials(_) => Text::AccountLoginSaved,
            AccountScreen::Nickname(_) => Text::NicknameSaved,
            AccountScreen::Email(purpose, step @ EmailStep::Address) => {
                *step = purpose.step(view);
                self.code.update(cx, |input, cx| input.set_value("", cx));
                return None;
            }
            AccountScreen::Email(EmailPurpose::Registration, EmailStep::Code) => {
                Text::AccountVerified
            }
            AccountScreen::Email(EmailPurpose::Profile(_), EmailStep::Code) => {
                Text::ProfileEmailSaved
            }
            AccountScreen::Overview => return None,
        };
        self.clear_secrets(cx);
        self.page = AccountScreen::Overview;
        self.feedback = None;
        Some(message)
    }
}
