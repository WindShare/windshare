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

    pub(super) fn setup_return(
        &self,
        theme: Theme,
        operation: &str,
        cx: &Context<Self>,
    ) -> Option<Div> {
        let Runtime::Native(session) = &self.runtime else {
            return None;
        };
        let setup = &session.model.setup;
        let context = setup.context()?;
        if !setup.unfinished() || operation.is_empty() || context.operation_id == operation {
            return None;
        }
        let original = self.shares.iter().find(|share|
            matches!(&share.source, crate::model::ShareSource::Native { operation_id } if operation_id == &context.operation_id));
        let locale = language(cx);
        let progress = match setup {
            Setup::Running {
                verification_url, ..
            } if !verification_url.is_empty() => Text::AuthorizationRequired,
            Setup::Cancelling { .. } => Text::CancellingSetup,
            Setup::Failed { .. } | Setup::CancelFailed { .. } => Text::SetupNeedsAttention,
            _ => Text::ConnectingRelay,
        };
        Some(
            column()
                .gap(px(12.))
                .child(theme.caption(locale.text(progress)))
                .when_some(original, |view, share| {
                    view.child(theme.label(share.name.clone(), 16.))
                })
                .child(Self::button(
                    "return-to-running-setup",
                    locale.text(Text::ReturnToSetup),
                    Icon::Settings,
                    ButtonStyle::Secondary,
                    Action::ReturnToSharingSetup,
                    cx,
                )),
        )
    }

    pub(super) fn setup_progress(theme: Theme, setup: &Setup, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let mut panel = column().gap(px(12.));
        match setup {
            Setup::Failed { error, .. } | Setup::CancelFailed { error, .. } => {
                panel = panel.child(
                    theme
                        .label(error.message.clone(), 15.)
                        .text_color(Tone::Rose.text(theme)),
                );
            }
            Setup::Cancelling { .. } => {
                panel = panel.child(theme.caption(locale.text(Text::CancellingSetup)));
            }
            Setup::Running {
                verification_url,
                user_code,
                ..
            } if !verification_url.is_empty() => {
                panel = panel
                    .child(theme.caption(locale.text(Text::RelayApprovalDetail)))
                    .child(
                        row()
                            .gap(px(12.))
                            .child(theme.heading(user_code.clone(), 18.))
                            .child(Self::button(
                                "approve-device",
                                locale.text(Text::ApproveDevice),
                                Icon::External,
                                ButtonStyle::Primary,
                                Action::ApproveRelay,
                                cx,
                            )),
                    );
            }
            Setup::Running { .. } => {
                panel = panel.child(theme.caption(locale.text(Text::ConnectingRelay)));
            }
            _ => return panel,
        }
        panel.child(
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
        )
    }
}
