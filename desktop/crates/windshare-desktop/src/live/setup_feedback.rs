use super::Runtime;
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::Action,
};
use gpui::{Div, prelude::*, px};
use windshare_host::Setup;
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, row};

impl Desktop {
    pub fn setup_cancellation_notice(theme: Theme, error: &str, cx: &Context<Self>) -> Div {
        theme
            .card()
            .p(px(18.))
            .gap(px(12.))
            .child(
                theme
                    .label(error.to_owned(), 15.)
                    .text_color(Tone::Rose.text(theme)),
            )
            .child(Self::button(
                "retry-setup-cancellation",
                language(cx).text(Text::RetrySetupCancellation),
                Icon::Refresh,
                ButtonStyle::Secondary,
                Action::CancelSharingSetup,
                cx,
            ))
    }

    pub(super) fn setup_feedback(&self, theme: Theme, operation: &str, cx: &Context<Self>) -> Div {
        let Runtime::Native(session) = &self.runtime else {
            return column();
        };
        let locale = language(cx);
        let mut form = column().gap(px(12.));
        let setup = &session.model.setup;
        let related = setup
            .context()
            .is_some_and(|context| operation.is_empty() || operation == context.operation_id);
        if related {
            if setup
                .context()
                .is_some_and(|context| context.frontend_saved)
                && !matches!(setup, Setup::Finished { .. })
            {
                form = form.child(theme.caption(locale.text(Text::FrontendSaved)));
            }
            match setup {
                Setup::Failed { error, .. } | Setup::CancelFailed { error, .. } => {
                    form = form.child(
                        theme
                            .label(error.message.clone(), 15.)
                            .text_color(Tone::Rose.text(theme)),
                    );
                }
                Setup::Finished { .. } => {
                    form = form.child(theme.caption(locale.text(Text::SettingsSaved)));
                }
                Setup::Cancelling { .. } | Setup::Cancelled { .. } => {
                    form = form.child(theme.caption(locale.text(if setup.busy() {
                        Text::CancellingSetup
                    } else {
                        Text::SetupCancelled
                    })));
                }
                Setup::Running {
                    verification_url,
                    user_code,
                    ..
                } if !verification_url.is_empty() => {
                    form = form.child(
                        row()
                            .gap(px(12.))
                            .child(theme.label(user_code.clone(), 18.))
                            .child(Self::button(
                                "approve-device",
                                locale.text(Text::ApproveDevice),
                                Icon::External,
                                ButtonStyle::Secondary,
                                Action::ApproveRelay,
                                cx,
                            )),
                    );
                }
                _ => {}
            }
            if setup.unfinished() {
                form = form.child(
                    Self::button(
                        "cancel-sharing-setup",
                        locale.text(if matches!(setup, Setup::CancelFailed { .. }) {
                            Text::RetrySetupCancellation
                        } else {
                            Text::CancelSetup
                        }),
                        Icon::Close,
                        ButtonStyle::Secondary,
                        Action::CancelSharingSetup,
                        cx,
                    )
                    .disabled(matches!(setup, Setup::Cancelling { .. })),
                );
            }
        }
        form
    }
}
